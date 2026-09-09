package persona_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/miere/murtaugh/internal/agent/persona"
)

func writeSoul(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, persona.File), []byte(body), 0o644); err != nil {
		t.Fatalf("write SOUL.md: %v", err)
	}
}

const seeded = "---\nname: Murtaugh\ndescription: voice\nkeep-coding-instructions: true\n---\n"

// The frontmatter is Claude Code output-style metadata. It is what lets the same
// file be selected from the interactive CLI, and it is exactly what must not
// reach a system prompt.
func TestStripFrontmatter(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"strips a frontmatter block", seeded + "\nI am Murtaugh.\n", "I am Murtaugh."},
		{"frontmatter only yields nothing", seeded, ""},
		{"no frontmatter is returned as-is", "I am Murtaugh.\n", "I am Murtaugh."},
		{"empty document", "", ""},
		{"leading blank lines before the fence", "\n\n" + seeded + "Voice.", "Voice."},
		{"a horizontal rule mid-document is not a fence", "Voice.\n\n---\n\nMore.", "Voice.\n\n---\n\nMore."},
		// An unterminated fence is far likelier to be prose that opens with a
		// rule than a truncated header; swallowing it would erase the voice.
		{"unterminated frontmatter keeps the whole document", "---\nname: x\nI am Murtaugh.", "---\nname: x\nI am Murtaugh."},
		{"a --- with trailing text is not a fence", "--- not frontmatter\nVoice.", "--- not frontmatter\nVoice."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := persona.StripFrontmatter(tc.in); got != tc.want {
				t.Fatalf("StripFrontmatter(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The workdir override is what lets one node give different agents different
// voices per context — the reason resolution is a search and not a single read.
func TestResolvePrefersWorkdirOverWorkspace(t *testing.T) {
	workspace, workdir := t.TempDir(), t.TempDir()
	writeSoul(t, workspace, seeded+"I am the node default.")
	writeSoul(t, workdir, seeded+"I am the workdir override.")

	if got := persona.Resolve("", workdir, workspace); got != "I am the workdir override." {
		t.Fatalf("workdir SOUL.md must win, got %q", got)
	}
	if got := persona.Resolve("", "", workspace); got != "I am the node default." {
		t.Fatalf("no workdir should fall back to the workspace default, got %q", got)
	}
}

// soul_file is the only knob separating two profiles that share one workdir,
// which the onboarding flow would otherwise collapse into a single voice.
func TestResolveProfileFileOutranksEverything(t *testing.T) {
	workspace, workdir := t.TempDir(), t.TempDir()
	writeSoul(t, workspace, seeded+"node default")
	writeSoul(t, workdir, seeded+"workdir override")
	if err := os.WriteFile(filepath.Join(workspace, "tweaker.md"), []byte(seeded+"I am the tweaker."), 0o644); err != nil {
		t.Fatalf("write tweaker.md: %v", err)
	}

	// A relative soul_file resolves against the workspace dir.
	if got := persona.Resolve("tweaker.md", workdir, workspace); got != "I am the tweaker." {
		t.Fatalf("relative soul_file = %q, want the tweaker persona", got)
	}
	// So does an absolute one.
	abs := filepath.Join(workspace, "tweaker.md")
	if got := persona.Resolve(abs, workdir, workspace); got != "I am the tweaker." {
		t.Fatalf("absolute soul_file = %q, want the tweaker persona", got)
	}
}

// Every step is best-effort: a bad path or a not-yet-onboarded file falls
// through rather than failing the agent's build.
func TestResolveFallsThrough(t *testing.T) {
	workspace, workdir := t.TempDir(), t.TempDir()
	writeSoul(t, workspace, seeded+"node default")

	// A soul_file pointing nowhere falls through to the search.
	if got := persona.Resolve("missing.md", workdir, workspace); got != "node default" {
		t.Fatalf("a missing soul_file should fall through, got %q", got)
	}
	// A workdir SOUL.md that is still frontmatter-only (seeded, not onboarded)
	// is not a voice, so resolution continues past it.
	writeSoul(t, workdir, seeded)
	if got := persona.Resolve("", workdir, workspace); got != "node default" {
		t.Fatalf("an un-onboarded workdir SOUL.md should fall through, got %q", got)
	}
	// Nothing anywhere is the pre-onboarding state, not an error.
	if got := persona.Resolve("", t.TempDir(), t.TempDir()); got != "" {
		t.Fatalf("expected no persona, got %q", got)
	}
	if got := persona.Resolve("", "", ""); got != "" {
		t.Fatalf("expected no persona with no dirs, got %q", got)
	}
}
