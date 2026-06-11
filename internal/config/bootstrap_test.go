package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/assets"
)

// assertEmbeddedTreeCopied walks the embedded srcRoot subtree and fails the
// test unless every file was mirrored, byte-for-byte, under dstRoot.
func assertEmbeddedTreeCopied(t *testing.T, srcRoot, dstRoot string) {
	t.Helper()
	err := fs.WalkDir(assets.FS, srcRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		want, err := assets.FS.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, srcRoot+"/")
		got, err := os.ReadFile(filepath.Join(dstRoot, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read bootstrapped %q: %v", rel, err)
		}
		if string(got) != string(want) {
			t.Fatalf("content mismatch for %q", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded %q: %v", srcRoot, err)
	}
}

func TestBootstrapFreshInstall(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "murtaugh")
	configPath := filepath.Join(baseDir, "slack.yaml")

	if err := Bootstrap(configPath); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	want, err := assets.FS.ReadFile("slack.yaml")
	if err != nil {
		t.Fatalf("read embedded slack.yaml: %v", err)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read bootstrapped slack.yaml: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("slack.yaml content mismatch: got %q want %q", got, want)
	}

	wantAgents, err := assets.FS.ReadFile("agents.yaml")
	if err != nil {
		t.Fatalf("read embedded agents.yaml: %v", err)
	}
	gotAgents, err := os.ReadFile(filepath.Join(baseDir, "agents.yaml"))
	if err != nil {
		t.Fatalf("read bootstrapped agents.yaml: %v", err)
	}
	if string(gotAgents) != string(wantAgents) {
		t.Fatalf("agents.yaml content mismatch")
	}

	// Every embedded template and skill file must be mirrored into the
	// workspace: templates under templates/, skills under .agents/skills/.
	assertEmbeddedTreeCopied(t, "templates", filepath.Join(baseDir, "templates"))
	assertEmbeddedTreeCopied(t, "skills", filepath.Join(baseDir, ".agents", "skills"))

	// .claude/skills is a relative symlink to .agents/skills so Claude-based
	// agents discover the same bundled skills.
	link := filepath.Join(baseDir, ".claude", "skills")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("expected .claude/skills to be a symlink: %v", err)
	}
	if want := filepath.Join("..", ".agents", "skills"); target != want {
		t.Fatalf("symlink target = %q, want %q", target, want)
	}
	// The symlink must resolve to the real skill tree.
	if _, err := os.Stat(filepath.Join(link, "murtaugh-slack", "SKILL.md")); err != nil {
		t.Fatalf("skill not reachable through .claude/skills symlink: %v", err)
	}

	// Optional docs are not embedded, so they must be skipped silently.
	for _, name := range optionalBootstrapDocs {
		if _, err := os.Stat(filepath.Join(baseDir, name)); !os.IsNotExist(err) {
			t.Fatalf("expected optional doc %q to be skipped, stat err=%v", name, err)
		}
	}
}

func TestBootstrapDoesNotOverwriteExistingFiles(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "murtaugh")
	configPath := filepath.Join(baseDir, "slack.yaml")
	skillDir := filepath.Join(baseDir, ".agents", "skills", "murtaugh-slack")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("seed dirs: %v", err)
	}

	const customConfig = "oauth:\n  app_token: keep-me\n"
	if err := os.WriteFile(configPath, []byte(customConfig), 0o644); err != nil {
		t.Fatalf("seed slack.yaml: %v", err)
	}

	existingSkill := filepath.Join(skillDir, "SKILL.md")
	const customSkill = "user authored skill"
	if err := os.WriteFile(existingSkill, []byte(customSkill), 0o644); err != nil {
		t.Fatalf("seed skill: %v", err)
	}

	if err := Bootstrap(configPath); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	if got, _ := os.ReadFile(configPath); string(got) != customConfig {
		t.Fatalf("slack.yaml was overwritten: got %q", got)
	}
	if got, _ := os.ReadFile(existingSkill); string(got) != customSkill {
		t.Fatalf("existing skill was overwritten: got %q", got)
	}
}

func TestBootstrapCopiesJobsYAML(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "murtaugh")
	configPath := filepath.Join(baseDir, "slack.yaml")

	if err := Bootstrap(configPath); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	wantJobs, err := assets.FS.ReadFile("jobs.yaml")
	if err != nil {
		t.Fatalf("read embedded jobs.yaml: %v", err)
	}
	gotJobs, err := os.ReadFile(filepath.Join(baseDir, "jobs.yaml"))
	if err != nil {
		t.Fatalf("read bootstrapped jobs.yaml: %v", err)
	}
	if string(gotJobs) != string(wantJobs) {
		t.Fatalf("jobs.yaml content mismatch: got %q want %q", gotJobs, wantJobs)
	}
}

func TestBootstrapDoesNotOverwriteExistingJobsYAML(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "murtaugh")
	configPath := filepath.Join(baseDir, "slack.yaml")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatalf("seed dir: %v", err)
	}

	const customJobs = "jobs:\n  my-job:\n    command: /bin/true\n"
	jobsPath := filepath.Join(baseDir, "jobs.yaml")
	if err := os.WriteFile(jobsPath, []byte(customJobs), 0o644); err != nil {
		t.Fatalf("seed jobs.yaml: %v", err)
	}

	if err := Bootstrap(configPath); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	if got, _ := os.ReadFile(jobsPath); string(got) != customJobs {
		t.Fatalf("jobs.yaml was overwritten: got %q", got)
	}
}

func TestBootstrapIsIdempotent(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "murtaugh", "slack.yaml")

	if err := Bootstrap(configPath); err != nil {
		t.Fatalf("first Bootstrap returned error: %v", err)
	}
	if err := Bootstrap(configPath); err != nil {
		t.Fatalf("second Bootstrap returned error: %v", err)
	}
}
