package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
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

// Bootstrap ensures the config directory containing configPath exists and is
// populated with the built-in defaults the first time the app runs. It is
// idempotent: existing files are never overwritten.
//
// On a fresh install it creates:
//   - the config directory (e.g. ~/.config/murtaugh)
//   - slack.yaml seeded with the default ping/pong configuration
//   - a skills/ directory holding every skill bundled in assets/skills
//   - AGENTS.md and BOOTSTRAP.md, when those docs are embedded in assets/
func Bootstrap(configPath string) error {
	baseDir := filepath.Dir(configPath)
	if err := os.MkdirAll(baseDir, bootstrapDirPerm); err != nil {
		return fmt.Errorf("create config dir %q: %w", baseDir, err)
	}

	if err := copyAssetFile("slack.yaml", configPath); err != nil {
		return err
	}
	if err := copyAssetFile("agents.yaml", filepath.Join(baseDir, "agents.yaml")); err != nil {
		return err
	}

	if err := copySkills(filepath.Join(baseDir, "skills")); err != nil {
		return err
	}

	for _, name := range optionalBootstrapDocs {
		if err := copyAssetFile(name, filepath.Join(baseDir, name)); err != nil {
			return err
		}
	}
	return nil
}

// copySkills mirrors every *.md skill from the embedded assets/skills directory
// into skillsDir, creating it on demand. Existing skills are left untouched.
func copySkills(skillsDir string) error {
	entries, err := assets.FS.ReadDir("skills")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read embedded skills: %w", err)
	}
	if err := os.MkdirAll(skillsDir, bootstrapDirPerm); err != nil {
		return fmt.Errorf("create skills dir %q: %w", skillsDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		src := path.Join("skills", entry.Name())
		dst := filepath.Join(skillsDir, entry.Name())
		if err := copyAssetFile(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// copyAssetFile writes the embedded asset src to dst. It never overwrites an
// existing dst, and silently skips when src is not present in the embedded FS
// so that optional assets remain optional.
func copyAssetFile(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat %q: %w", dst, err)
	}

	data, err := assets.FS.ReadFile(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read embedded asset %q: %w", src, err)
	}

	if err := os.WriteFile(dst, data, bootstrapFilePerm); err != nil {
		return fmt.Errorf("write %q: %w", dst, err)
	}
	return nil
}
