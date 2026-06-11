package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/miere/murtaugh-dev-toolkit/assets"
)

const (
	bootstrapDirPerm  = 0o755
	bootstrapFilePerm = 0o644
)

// optionalBootstrapDocs are copied from the embedded assets into the config
// directory on first run when present. They are skipped silently when the
// asset is not bundled, satisfying the "skip if they don't exist" convention.
var optionalBootstrapDocs = []string{"AGENTS.md", "BOOTSTRAP.md"}

// BootstrapReport summarises the result of a Bootstrap pass: which on-disk
// files were written for the first time, and which were preserved because
// they already existed. Paths are absolute. Optional assets that are absent
// from the embedded FS are reported in neither bucket.
type BootstrapReport struct {
	Created   []string
	Preserved []string
}

// Bootstrap ensures the config directory containing configPath exists and is
// populated with the built-in defaults the first time the app runs. It is
// idempotent: existing files are never overwritten. The returned report is
// discarded; callers that need it should use BootstrapWithReport instead.
func Bootstrap(configPath string) error {
	_, err := BootstrapWithReport(configPath)
	return err
}

// BootstrapWithReport is the report-returning variant of Bootstrap. It
// performs the same idempotent seeding and, on success, returns a report
// describing what was created and what was preserved.
//
// On a fresh install it creates, under the workspace directory (the directory
// holding configPath, e.g. ~/.config/murtaugh):
//   - slack.yaml seeded with the default ping/pong configuration, plus
//     agents.yaml and jobs.yaml
//   - templates/ — the bundled Block Kit templates (ping/, unfurl/)
//   - .agents/skills/ — every bundled agent skill (SKILL.md + reference/ +
//     examples/), mirrored recursively
//   - .claude/skills — a symlink to .agents/skills so Claude-based agents
//     discover the same skills without a second copy
//   - AGENTS.md and BOOTSTRAP.md, when those docs are embedded in assets/
func BootstrapWithReport(configPath string) (BootstrapReport, error) {
	report := BootstrapReport{}
	baseDir := filepath.Dir(configPath)
	if err := os.MkdirAll(baseDir, bootstrapDirPerm); err != nil {
		return report, fmt.Errorf("create config dir %q: %w", baseDir, err)
	}

	plan := []struct{ src, dst string }{
		{"slack.yaml", configPath},
		{"agents.yaml", filepath.Join(baseDir, "agents.yaml")},
		{"jobs.yaml", filepath.Join(baseDir, "jobs.yaml")},
	}
	for _, name := range optionalBootstrapDocs {
		plan = append(plan, struct{ src, dst string }{name, filepath.Join(baseDir, name)})
	}
	for _, entry := range plan {
		outcome, err := copyAssetFile(entry.src, entry.dst)
		if err != nil {
			return report, err
		}
		report.absorb(outcome, entry.dst)
	}

	// Block Kit templates land at <workspace>/templates/...; the agent skills
	// land at <workspace>/.agents/skills/... Both mirror the embedded tree
	// recursively and never overwrite a file the user has edited.
	treeRoots := []struct{ src, dst string }{
		{"templates", filepath.Join(baseDir, "templates")},
		{"skills", filepath.Join(baseDir, ".agents", "skills")},
	}
	for _, root := range treeRoots {
		outcomes, err := copyAssetTree(root.src, root.dst)
		if err != nil {
			return report, err
		}
		for _, item := range outcomes {
			report.absorb(item.outcome, item.path)
		}
	}

	link, err := linkClaudeSkills(baseDir)
	if err != nil {
		return report, err
	}
	report.absorb(link.outcome, link.path)

	return report, nil
}

// copyOutcome describes what happened to a single destination path during a
// bootstrap pass.
type copyOutcome int

const (
	copyOutcomeSkippedMissingAsset copyOutcome = iota
	copyOutcomeCreated
	copyOutcomePreserved
)

// skillCopyResult pairs a destination path with its outcome so the caller
// can report skills/* paths individually.
type skillCopyResult struct {
	path    string
	outcome copyOutcome
}

func (r *BootstrapReport) absorb(outcome copyOutcome, dst string) {
	switch outcome {
	case copyOutcomeCreated:
		r.Created = append(r.Created, dst)
	case copyOutcomePreserved:
		r.Preserved = append(r.Preserved, dst)
	}
}

// copyAssetTree mirrors the embedded directory srcRoot into dstRoot,
// preserving the subtree structure. dstRoot is created even when the tree is
// empty so a dependent symlink (see linkClaudeSkills) never dangles. Existing
// destination files are preserved verbatim — the same non-destructive policy
// as copyAssetFile — so user edits survive re-bootstrap. A missing embedded
// srcRoot is tolerated (returns no results), keeping the asset optional.
func copyAssetTree(srcRoot, dstRoot string) ([]skillCopyResult, error) {
	if _, err := fs.Stat(assets.FS, srcRoot); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err := os.MkdirAll(dstRoot, bootstrapDirPerm); err != nil {
		return nil, fmt.Errorf("create dir %q: %w", dstRoot, err)
	}
	var results []skillCopyResult
	walkErr := fs.WalkDir(assets.FS, srcRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, srcRoot+"/")
		dst := filepath.Join(dstRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), bootstrapDirPerm); err != nil {
			return fmt.Errorf("create dir %q: %w", filepath.Dir(dst), err)
		}
		outcome, err := copyAssetFile(p, dst)
		if err != nil {
			return err
		}
		results = append(results, skillCopyResult{path: dst, outcome: outcome})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("copy embedded tree %q: %w", srcRoot, walkErr)
	}
	return results, nil
}

// linkClaudeSkills creates <baseDir>/.claude/skills as a relative symlink to
// the sibling .agents/skills directory, so Claude-based agents discover the
// same bundled skills the ACP agents do without a duplicate copy. It is
// non-destructive and idempotent: when anything already exists at the link
// path (a prior symlink, a real directory, a file) it is preserved untouched.
func linkClaudeSkills(baseDir string) (skillCopyResult, error) {
	link := filepath.Join(baseDir, ".claude", "skills")
	if _, err := os.Lstat(link); err == nil {
		return skillCopyResult{path: link, outcome: copyOutcomePreserved}, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return skillCopyResult{}, fmt.Errorf("stat %q: %w", link, err)
	}
	if err := os.MkdirAll(filepath.Dir(link), bootstrapDirPerm); err != nil {
		return skillCopyResult{}, fmt.Errorf("create dir %q: %w", filepath.Dir(link), err)
	}
	// Relative target resolved from the link's own directory (.claude/):
	// ../.agents/skills → <baseDir>/.agents/skills.
	target := filepath.Join("..", ".agents", "skills")
	if err := os.Symlink(target, link); err != nil {
		return skillCopyResult{}, fmt.Errorf("symlink %q -> %q: %w", link, target, err)
	}
	return skillCopyResult{path: link, outcome: copyOutcomeCreated}, nil
}

// copyAssetFile writes the embedded asset src to dst. It never overwrites an
// existing dst, and silently skips when src is not present in the embedded FS
// so that optional assets remain optional. The returned outcome distinguishes
// between a fresh write, a preserved file, and a missing optional asset.
func copyAssetFile(src, dst string) (copyOutcome, error) {
	if _, err := os.Stat(dst); err == nil {
		return copyOutcomePreserved, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return copyOutcomeSkippedMissingAsset, fmt.Errorf("stat %q: %w", dst, err)
	}

	data, err := assets.FS.ReadFile(src)
	if errors.Is(err, fs.ErrNotExist) {
		return copyOutcomeSkippedMissingAsset, nil
	}
	if err != nil {
		return copyOutcomeSkippedMissingAsset, fmt.Errorf("read embedded asset %q: %w", src, err)
	}

	if err := os.WriteFile(dst, data, bootstrapFilePerm); err != nil {
		return copyOutcomeSkippedMissingAsset, fmt.Errorf("write %q: %w", dst, err)
	}
	return copyOutcomeCreated, nil
}
