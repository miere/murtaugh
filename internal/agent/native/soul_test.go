package native

import (
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

// The persona is appended AFTER the base prompt, not before it: the base is
// operational scaffolding and the persona is voice, so the voice gets recency
// rather than being argued down by the tool discipline that follows.
func TestAppendPersona(t *testing.T) {
	got := AppendPersona("Follow the rules.", "You are Murtaugh.")
	if !strings.HasSuffix(got, "<persona>\nYou are Murtaugh.\n</persona>") {
		t.Fatalf("persona block not appended: %q", got)
	}
	if !strings.HasPrefix(got, "Follow the rules.") {
		t.Fatalf("base prompt must lead: %q", got)
	}
	// An empty persona — the seeded, frontmatter-only SOUL.md of an agent that
	// has not onboarded yet — leaves the base untouched.
	if got := AppendPersona("base", ""); got != "base" {
		t.Fatalf("empty persona changed base: %q", got)
	}
	// Empty base with a persona yields just the block.
	if got := AppendPersona("", "Hi"); got != "<persona>\nHi\n</persona>" {
		t.Fatalf("unexpected persona-only output: %q", got)
	}
}

// The gateway turns an agent's Markdown into Slack's format itself, so the
// prompt must not teach the model a Slack dialect it no longer has to write.
func TestBuildLeavesSlackFormattingOutOfThePrompt(t *testing.T) {
	t.Setenv("TEST_FORMAT_KEY", "x")
	base := t.TempDir()
	c, err := Build(config.AgentProfile{
		WorkDir: base,
		Native:  &config.NativeProfile{Provider: "gemini", Model: "gemini-2.5-pro", APIKeyEnv: "TEST_FORMAT_KEY"},
	}, BuildDeps{WorkspaceDir: base, Root: rootFor(t, base)})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	system := BuildSystemPrompt(c.systemPrompt, c.agentsDoc, c.skillsIndex)
	for _, gone := range []string{"Formatting for Slack", "mrkdwn", "Formatting rules are appended"} {
		if strings.Contains(system, gone) {
			t.Fatalf("the system prompt still carries Slack formatting (%q):\n%s", gone, system)
		}
	}
}
