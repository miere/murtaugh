package native

import (
	"strings"
	"testing"
)

// The dialect rule describes the transport, not the agent, so it has to reach
// the model whatever the operator did to the system prompt.
func TestAppendSlackFormatAlwaysAppendsTheRules(t *testing.T) {
	for _, base := range []string{"", "You are a helpful agent.", "trailing newlines\n\n\n"} {
		got := AppendSlackFormat(base)

		if !strings.Contains(got, "Formatting for Slack") {
			t.Fatalf("base %q: rules missing from\n%s", base, got)
		}
		if trimmed := strings.TrimSpace(base); trimmed != "" && !strings.HasPrefix(got, trimmed) {
			t.Fatalf("base %q: original prompt no longer leads", base)
		}
	}
}

// The two dialects and which surface takes which are the whole point of the
// file; a rewrite that drops either half silently reopens the bug.
func TestSlackFormatNamesBothDialects(t *testing.T) {
	rules := AppendSlackFormat("")
	for _, want := range []string{"standard Markdown", "mrkdwn", "Block Kit", "<@U123>"} {
		if !strings.Contains(rules, want) {
			t.Fatalf("formatting rules no longer mention %q:\n%s", want, rules)
		}
	}
}
