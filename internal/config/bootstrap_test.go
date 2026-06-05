package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/assets"
)

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

	// Every embedded skill must be mirrored into the skills/ directory.
	entries, err := assets.FS.ReadDir("skills")
	if err != nil {
		t.Fatalf("read embedded skills: %v", err)
	}
	for _, entry := range entries {
		wantSkill, err := assets.FS.ReadFile("skills/" + entry.Name())
		if err != nil {
			t.Fatalf("read embedded skill %q: %v", entry.Name(), err)
		}
		gotSkill, err := os.ReadFile(filepath.Join(baseDir, "skills", entry.Name()))
		if err != nil {
			t.Fatalf("read bootstrapped skill %q: %v", entry.Name(), err)
		}
		if string(gotSkill) != string(wantSkill) {
			t.Fatalf("skill %q content mismatch", entry.Name())
		}
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
	skillsDir := filepath.Join(baseDir, "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatalf("seed dirs: %v", err)
	}

	const customConfig = "slack:\n  app_token: keep-me\n"
	if err := os.WriteFile(configPath, []byte(customConfig), 0o644); err != nil {
		t.Fatalf("seed slack.yaml: %v", err)
	}

	entries, err := assets.FS.ReadDir("skills")
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected embedded skills, err=%v entries=%d", err, len(entries))
	}
	existingSkill := filepath.Join(skillsDir, entries[0].Name())
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

func TestBootstrapIsIdempotent(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "murtaugh", "slack.yaml")

	if err := Bootstrap(configPath); err != nil {
		t.Fatalf("first Bootstrap returned error: %v", err)
	}
	if err := Bootstrap(configPath); err != nil {
		t.Fatalf("second Bootstrap returned error: %v", err)
	}
}
