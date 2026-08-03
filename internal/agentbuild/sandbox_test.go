package agentbuild

import (
	"runtime"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

func resolvedProfile(t *testing.T, profile config.AgentProfile) ResolvedAgent {
	t.Helper()
	resolved, err := Resolve("probe", profile, t.TempDir())
	if err != nil {
		t.Fatalf("resolve agent: %v", err)
	}
	return resolved
}

// A native agent holds its toolset in-process — there is no spawned process to
// confine, and a sandbox block on one is unused rather than an error.
func TestResolveSandboxSkipsNativeAgents(t *testing.T) {
	profile := config.AgentProfile{
		Native:  &config.NativeProfile{Provider: "anthropic", Model: "claude-opus-5"},
		Sandbox: config.SandboxConfig{Mode: config.SandboxModeSeatbelt},
	}

	box, err := resolveSandbox(profile, resolvedProfile(t, profile), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if box != nil {
		t.Fatal("a native agent must not get a sandbox")
	}
}

// The typed-nil trap: returning a nil *sandbox.Plan directly would produce a
// NON-nil agent.Sandbox interface, silently routing every unsandboxed agent down
// the confined path. The seam must hand back a genuinely nil interface.
func TestResolveSandboxOffReturnsNilInterface(t *testing.T) {
	profile := config.AgentProfile{ClaudeCode: &config.ClaudeCodeProfile{Command: "claude"}}

	box, err := resolveSandbox(profile, resolvedProfile(t, profile), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if box != nil {
		t.Fatal("mode off must yield a nil agent.Sandbox interface, not a typed nil")
	}
}

// Fail closed: an unsupported platform or a missing sandbox-exec must abort the
// build rather than quietly spawning the agent unconfined.
func TestResolveSandboxFailsClosed(t *testing.T) {
	profile := config.AgentProfile{
		ClaudeCode: &config.ClaudeCodeProfile{Command: "claude"},
		Sandbox:    config.SandboxConfig{Mode: config.SandboxModeSeatbelt},
	}

	box, err := resolveSandbox(profile, resolvedProfile(t, profile), nil)

	if runtime.GOOS == "darwin" {
		if err != nil {
			t.Fatalf("seatbelt should resolve on macOS: %v", err)
		}
		if box == nil {
			t.Fatal("expected a sandbox for mode seatbelt")
		}
		return
	}
	if err == nil {
		t.Fatal("seatbelt on a non-macOS host must be an error, never a silent unconfined spawn")
	}
	if box != nil {
		t.Fatal("a failed sandbox resolution must not yield a usable box")
	}
	if !strings.Contains(err.Error(), "probe") {
		t.Fatalf("error should name the agent, got: %v", err)
	}
}

func TestResolveSandboxRejectsUnknownMode(t *testing.T) {
	profile := config.AgentProfile{
		ClaudeCode: &config.ClaudeCodeProfile{Command: "claude"},
		Sandbox:    config.SandboxConfig{Mode: "bwrap"},
	}

	if _, err := resolveSandbox(profile, resolvedProfile(t, profile), nil); err == nil {
		t.Fatal("expected an error for an unsupported mode")
	}
}

// Config validation is the earlier of the two gates: a bad mode should be caught
// at load time, not only when an agent is built.
func TestProfileValidateRejectsUnknownSandboxMode(t *testing.T) {
	profile := config.AgentProfile{Sandbox: config.SandboxConfig{Mode: "firejail"}}

	if err := profile.Validate(); err == nil {
		t.Fatal("Validate must reject an unknown sandbox mode")
	}
}
