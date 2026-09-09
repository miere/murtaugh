package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The seeded SOUL.md is what makes onboarding an EDIT rather than a create.
// AGENTS.md tells the agent to write its voice in "preserving the existing
// frontmatter", and a file that does not exist has no frontmatter to preserve —
// an agent left to invent it would eventually write its own name into `name:`,
// the binding key Claude Code's outputStyle matches on.
func TestScaffoldWorkspaceDocsSeedsTheOnboardingFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := ScaffoldWorkspaceDocs(dir); err != nil {
		t.Fatalf("ScaffoldWorkspaceDocs: %v", err)
	}

	soul, err := os.ReadFile(filepath.Join(dir, PersonaFile))
	if err != nil {
		t.Fatalf("read SOUL.md: %v", err)
	}
	if !strings.HasPrefix(string(soul), "---") || !strings.Contains(string(soul), "name: Murtaugh") {
		t.Fatalf("seeded SOUL.md must carry the output-style frontmatter:\n%s", soul)
	}

	agents, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if !strings.Contains(string(agents), "I don't have a name or personality yet") {
		t.Fatalf("seeded AGENTS.md must carry the onboarding steps:\n%s", agents)
	}

	// AGENTS.md links to GUIDELINES.md from its first section, so the file has to
	// exist before onboarding fills it in or that link dangles from day one.
	if _, err := os.Stat(filepath.Join(dir, GuidelinesFile)); err != nil {
		t.Fatalf("GUIDELINES.md not seeded: %v", err)
	}
}

// Both links serve the interactive `claude` CLI: CLAUDE.md so the docs load as
// project memory, the output style so the agent's voice appears in the admin's
// style list. Murtaugh's own path reads SOUL.md directly, so neither is
// load-bearing for the daemon.
func TestScaffoldWorkspaceDocsLinksTheClaudeAliases(t *testing.T) {
	dir := t.TempDir()
	if _, err := ScaffoldWorkspaceDocs(dir); err != nil {
		t.Fatalf("ScaffoldWorkspaceDocs: %v", err)
	}

	for _, tc := range []struct{ link, wantTarget, wantSameAs string }{
		{filepath.Join(dir, "CLAUDE.md"), "AGENTS.md", filepath.Join(dir, "AGENTS.md")},
		{filepath.Join(dir, ".claude", "output-styles", "murtaugh.md"), filepath.Join("..", "..", PersonaFile), filepath.Join(dir, PersonaFile)},
	} {
		target, err := os.Readlink(tc.link)
		if err != nil {
			t.Fatalf("readlink %s: %v", tc.link, err)
		}
		if target != tc.wantTarget {
			t.Fatalf("%s -> %q, want %q", tc.link, target, tc.wantTarget)
		}
		// The relative target must actually resolve, not just look right.
		got, err := os.ReadFile(tc.link)
		if err != nil {
			t.Fatalf("link %s does not resolve: %v", tc.link, err)
		}
		want, err := os.ReadFile(tc.wantSameAs)
		if err != nil {
			t.Fatalf("read %s: %v", tc.wantSameAs, err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s resolves to the wrong file", tc.link)
		}
	}
}

// Scaffolding runs on every agent build and may be pointed at a directory that
// is somebody's project repo, so it must never overwrite what it finds.
func TestScaffoldWorkspaceDocsIsNonDestructive(t *testing.T) {
	dir := t.TempDir()
	existing := map[string]string{
		"AGENTS.md":    "# My own project doc\n",
		PersonaFile:    "---\nname: Carmen\n---\nI am Carmen.\n",
		GuidelinesFile: "worktrees only\n",
		"CLAUDE.md":    "# A real file, not a link\n",
	}
	for name, body := range existing {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	if _, err := ScaffoldWorkspaceDocs(dir); err != nil {
		t.Fatalf("ScaffoldWorkspaceDocs: %v", err)
	}

	for name, want := range existing {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Fatalf("%s was overwritten:\ngot  %q\nwant %q", name, got, want)
		}
	}
	// A real CLAUDE.md must not have been replaced by a symlink.
	info, err := os.Lstat(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("lstat CLAUDE.md: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("an existing CLAUDE.md was replaced with a symlink")
	}

	// And running twice changes nothing.
	if _, err := ScaffoldWorkspaceDocs(dir); err != nil {
		t.Fatalf("second ScaffoldWorkspaceDocs: %v", err)
	}
}

func TestScaffoldWorkspaceDocsIgnoresAnEmptyDir(t *testing.T) {
	results, err := ScaffoldWorkspaceDocs("  ")
	if err != nil {
		t.Fatalf("blank workdir should be a no-op, got %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no results, got %+v", results)
	}
}
