package agent

import (
	"os"
	"strings"
	"testing"
)

func TestSpawnEnvStripsNestedClaudeMarker(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	os.Setenv("SPAWN_ENV_TEST_MARKER", "keep")
	t.Cleanup(func() { os.Unsetenv("SPAWN_ENV_TEST_MARKER") })

	env := SpawnEnv([]string{"FOO=bar"})

	var sawMarker, sawOverride, sawInherited bool
	for _, kv := range env {
		switch kv {
		case "CLAUDECODE=1":
			sawMarker = true
		case "FOO=bar":
			sawOverride = true
		case "SPAWN_ENV_TEST_MARKER=keep":
			sawInherited = true
		}
	}
	if sawMarker {
		t.Fatal("SpawnEnv must strip the nested-Claude-Code marker")
	}
	if !sawOverride {
		t.Fatal("SpawnEnv dropped the override")
	}
	if !sawInherited {
		t.Fatal("SpawnEnv dropped an ordinary inherited var")
	}
}

// fakeSandbox is a Sandbox with a fixed allowlist, so the env plumbing can be
// tested without a real (macOS-only) seatbelt plan.
type fakeSandbox struct{ allow []string }

func (f fakeSandbox) Wrap(command string, args []string) (string, []string) {
	return "wrapper", append([]string{command}, args...)
}
func (f fakeSandbox) EnvAllowlist() []string { return f.allow }

// A nil Sandbox must be byte-for-byte the pre-sandbox behaviour: unsandboxed
// agents are the default and must not change at all.
func TestSpawnEnvForNilSandboxInheritsEverything(t *testing.T) {
	os.Setenv("SPAWN_ENV_TEST_UNLISTED", "keep")
	t.Cleanup(func() { os.Unsetenv("SPAWN_ENV_TEST_UNLISTED") })

	if !hasEntry(SpawnEnvFor(nil, nil), "SPAWN_ENV_TEST_UNLISTED=keep") {
		t.Fatal("a nil sandbox must inherit the whole environment")
	}
}

func TestSpawnEnvForFiltersToAllowlist(t *testing.T) {
	os.Setenv("SPAWN_ENV_TEST_SECRET", "leak")
	os.Setenv("SPAWN_ENV_TEST_KEPT", "fine")
	t.Cleanup(func() {
		os.Unsetenv("SPAWN_ENV_TEST_SECRET")
		os.Unsetenv("SPAWN_ENV_TEST_KEPT")
	})

	env := SpawnEnvFor(fakeSandbox{allow: []string{"SPAWN_ENV_TEST_KEPT"}}, nil)

	if hasEntry(env, "SPAWN_ENV_TEST_SECRET=leak") {
		t.Fatal("an unlisted variable crossed into the sandbox")
	}
	if !hasEntry(env, "SPAWN_ENV_TEST_KEPT=fine") {
		t.Fatal("an allowlisted variable was dropped")
	}
}

// The profile's own env: map is applied AFTER the filter and unconditionally —
// that is what lets an operator inject a credential without widening the
// allowlist to let it through.
func TestSpawnEnvForOverridesBypassTheAllowlist(t *testing.T) {
	env := SpawnEnvFor(fakeSandbox{allow: []string{"PATH"}}, []string{"ANTHROPIC_API_KEY=injected"})

	if !hasEntry(env, "ANTHROPIC_API_KEY=injected") {
		t.Fatal("a profile env override was filtered out by the allowlist")
	}
}

// The whole CLAUDE_CODE_* family disappears under an allowlist, which subsumes
// the nested-marker strip for sandboxed agents.
func TestSpawnEnvForDropsNestedClaudeFamily(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")

	env := SpawnEnvFor(fakeSandbox{allow: []string{"PATH"}}, nil)

	for _, kv := range env {
		if strings.HasPrefix(kv, "CLAUDE") {
			t.Fatalf("nested-Claude marker survived the allowlist: %q", kv)
		}
	}
}

func TestWrapCommandNilSandboxIsIdentity(t *testing.T) {
	cmd, args := WrapCommand(nil, "claude", []string{"-p"})
	if cmd != "claude" || len(args) != 1 || args[0] != "-p" {
		t.Fatalf("nil sandbox altered the invocation: %q %v", cmd, args)
	}
}

func hasEntry(env []string, want string) bool {
	for _, kv := range env {
		if kv == want {
			return true
		}
	}
	return false
}
