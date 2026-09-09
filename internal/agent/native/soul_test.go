package native

import (
	"strings"
	"testing"
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

// Assembly order is base → persona → Slack rules, and the transport rules stay
// last so they survive an operator who replaces the whole system prompt.
func TestPersonaSitsBetweenBaseAndSlackRules(t *testing.T) {
	got := AppendSlackFormat(AppendPersona("BASE-MARKER", "SOUL-MARKER"))
	base := strings.Index(got, "BASE-MARKER")
	soul := strings.Index(got, "SOUL-MARKER")
	rules := strings.Index(got, "Formatting for Slack")
	if base < 0 || soul < 0 || rules < 0 {
		t.Fatalf("a section went missing: base=%d soul=%d rules=%d\n%s", base, soul, rules, got)
	}
	if !(base < soul && soul < rules) {
		t.Fatalf("wrong order (want base < persona < rules): base=%d soul=%d rules=%d", base, soul, rules)
	}
}
