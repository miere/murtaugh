package credwarden

import (
	"slices"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

func TestClaudeCodeIdentitiesIgnoresOtherBackends(t *testing.T) {
	agents := map[string]config.AgentProfile{
		"native": {Native: &config.NativeProfile{Provider: "gemini", Model: "m", APIKeyEnv: "K"}},
		"acp":    {ACP: &config.ACPProfile{Command: "/opt/claude-acp-bridge"}},
		"claude": {ClaudeCode: &config.ClaudeCodeProfile{Command: "/usr/local/bin/claude"}},
	}

	got := ClaudeCodeIdentities(agents)
	if len(got) != 1 {
		t.Fatalf("expected only the claude_code agent to be watched, got %d: %v", len(got), got)
	}
	if got[0].Command != "/usr/local/bin/claude" {
		t.Fatalf("unexpected command %q", got[0].Command)
	}
	if got[0].Home != "" {
		t.Fatalf("expected an inherited HOME, got %q", got[0].Home)
	}
}

func TestClaudeCodeIdentitiesCollapseToOneWatcherPerCredential(t *testing.T) {
	agents := map[string]config.AgentProfile{
		"a": {ClaudeCode: &config.ClaudeCodeProfile{Command: "/usr/local/bin/claude"}},
		"b": {ClaudeCode: &config.ClaudeCodeProfile{Command: "/usr/local/bin/claude"}},
		"c": {ClaudeCode: &config.ClaudeCodeProfile{Command: "/usr/local/bin/claude"}},
	}

	w := New(Options{Identities: ClaudeCodeIdentities(agents)})
	if w == nil {
		t.Fatal("expected a warden for three claude_code agents")
	}
	if got := w.Identities(); len(got) != 1 {
		t.Fatalf("expected 3 profiles to collapse to 1 credential, got %d: %v", len(got), got)
	}
}

func TestClaudeCodeIdentitiesSeparatesHomeOverride(t *testing.T) {
	agents := map[string]config.AgentProfile{
		"default": {ClaudeCode: &config.ClaudeCodeProfile{Command: "/usr/local/bin/claude"}},
		"tenant": {ClaudeCode: &config.ClaudeCodeProfile{
			Command: "/usr/local/bin/claude",
			Env:     map[string]string{"HOME": "/srv/tenant"},
		}},
	}

	got := New(Options{Identities: ClaudeCodeIdentities(agents)}).Identities()
	if len(got) != 2 {
		t.Fatalf("expected a HOME override to be a distinct credential, got %d: %v", len(got), got)
	}
	var homes []string
	for _, id := range got {
		homes = append(homes, id.Home)
	}
	if !slices.Contains(homes, "/srv/tenant") || !slices.Contains(homes, "") {
		t.Fatalf("expected both the inherited and overridden HOME, got %v", homes)
	}
}

func TestClaudeCodeIdentitiesSkipsBlankCommand(t *testing.T) {
	agents := map[string]config.AgentProfile{
		"broken": {ClaudeCode: &config.ClaudeCodeProfile{Command: "   "}},
	}
	if got := ClaudeCodeIdentities(agents); len(got) != 0 {
		t.Fatalf("expected no identity for a blank command, got %v", got)
	}
}

func TestNoClaudeCodeAgentYieldsNoWarden(t *testing.T) {
	agents := map[string]config.AgentProfile{
		"native": {Native: &config.NativeProfile{Provider: "gemini", Model: "m", APIKeyEnv: "K"}},
	}
	if w := New(Options{Identities: ClaudeCodeIdentities(agents)}); w != nil {
		t.Fatal("expected no warden when no claude_code agent is configured")
	}
}
