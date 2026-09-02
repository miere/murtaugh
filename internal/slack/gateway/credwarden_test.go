package gateway

import (
	"context"
	"testing"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/credwarden"
)

func TestClaudeCodeIdentitiesIgnoresOtherBackends(t *testing.T) {
	agents := map[string]config.AgentProfile{
		"native": {Native: &config.NativeProfile{Provider: "gemini", Model: "m", APIKeyEnv: "K"}},
		// An ACP agent may well be Claude Code behind an adapter, but the adapter's
		// command is arbitrary. Guessing would be a heuristic that fails quietly, so
		// ACP credentials are the admin's responsibility by decision.
		"acp":    {ACP: &config.ACPProfile{Command: "/opt/claude-acp-bridge"}},
		"claude": {ClaudeCode: &config.ClaudeCodeProfile{Command: "/usr/local/bin/claude"}},
	}

	got := claudeCodeIdentities(agents)
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

// N claude_code profiles pointing at one binary share ONE credential. Two
// wardens refreshing it concurrently would have the second present a token the
// first just retired — the exact failure the warden exists to prevent.
func TestClaudeCodeIdentitiesCollapseToOneWatcherPerCredential(t *testing.T) {
	agents := map[string]config.AgentProfile{
		"a": {ClaudeCode: &config.ClaudeCodeProfile{Command: "/usr/local/bin/claude"}},
		"b": {ClaudeCode: &config.ClaudeCodeProfile{Command: "/usr/local/bin/claude"}},
		"c": {ClaudeCode: &config.ClaudeCodeProfile{Command: "/usr/local/bin/claude"}},
	}

	w := credwarden.New(credwarden.Options{Identities: claudeCodeIdentities(agents)})
	if w == nil {
		t.Fatal("expected a warden for three claude_code agents")
	}
	if got := w.Identities(); len(got) != 1 {
		t.Fatalf("expected 3 profiles to collapse to 1 credential, got %d: %v", len(got), got)
	}
}

// A profile CAN redirect HOME through its env map, which points it at a
// different credential file. Missing that would leave the agent watched under
// the wrong identity, or silently sharing a watcher with a different store.
func TestClaudeCodeIdentitiesSeparatesHomeOverride(t *testing.T) {
	agents := map[string]config.AgentProfile{
		"default": {ClaudeCode: &config.ClaudeCodeProfile{Command: "/usr/local/bin/claude"}},
		"tenant": {ClaudeCode: &config.ClaudeCodeProfile{
			Command: "/usr/local/bin/claude",
			Env:     map[string]string{"HOME": "/srv/tenant"},
		}},
	}

	w := credwarden.New(credwarden.Options{Identities: claudeCodeIdentities(agents)})
	got := w.Identities()
	if len(got) != 2 {
		t.Fatalf("expected a HOME override to be a distinct credential, got %d: %v", len(got), got)
	}

	var homes []string
	for _, id := range got {
		homes = append(homes, id.Home)
	}
	if !hasString(homes, "/srv/tenant") || !hasString(homes, "") {
		t.Fatalf("expected both the inherited and overridden HOME, got %v", homes)
	}
}

func TestClaudeCodeIdentitiesSkipsBlankCommand(t *testing.T) {
	agents := map[string]config.AgentProfile{
		"broken": {ClaudeCode: &config.ClaudeCodeProfile{Command: "   "}},
	}
	if got := claudeCodeIdentities(agents); len(got) != 0 {
		t.Fatalf("expected no identity for a blank command, got %v", got)
	}
}

// No claude_code agent means no warden at all — the gate is the existence of a
// profile, not a config flag an operator can forget.
func TestNoClaudeCodeAgentYieldsNoWarden(t *testing.T) {
	agents := map[string]config.AgentProfile{
		"native": {Native: &config.NativeProfile{Provider: "gemini", Model: "m", APIKeyEnv: "K"}},
	}
	if w := credwarden.New(credwarden.Options{Identities: claudeCodeIdentities(agents)}); w != nil {
		t.Fatal("expected no warden when no claude_code agent is configured")
	}
}

func TestStartBackgroundIsNoOpWithoutAWarden(t *testing.T) {
	g := &Gateway{}
	g.StartBackground(context.Background()) // no claude_code agent: nothing to run
	g.StopBackground()                      // and stopping what never started is safe
}

// A configuration reload builds a replacement gateway. If the outgoing one kept
// its warden, two would run against the same credential and race the server's
// rotation of the refresh token — the failure the warden exists to prevent.
func TestStopBackgroundIsIdempotent(t *testing.T) {
	g := &Gateway{credWarden: credwarden.New(credwarden.Options{
		Identities: []credwarden.Identity{{Command: "/bin/claude"}},
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g.StartBackground(ctx)
	g.StartBackground(ctx) // second call must not start a second warden
	g.StopBackground()
	g.StopBackground() // and stopping twice must not panic
}

func hasString(in []string, want string) bool {
	for _, s := range in {
		if s == want {
			return true
		}
	}
	return false
}
