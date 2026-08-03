package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSmokeRealClaudeInsideTheBox runs the actual `claude` binary under a
// generated profile, with the real inherited-env allowlist applied. Everything
// else in this package tests the mechanism; this tests the thing that matters —
// whether Claude Code still works once confined.
//
// It is opt-in (MURTAUGH_SANDBOX_SMOKE=1) because it needs a logged-in claude and
// makes a live API call.
func TestSmokeRealClaudeInsideTheBox(t *testing.T) {
	if os.Getenv("MURTAUGH_SANDBOX_SMOKE") != "1" {
		t.Skip("set MURTAUGH_SANDBOX_SMOKE=1 to run the live claude smoke test")
	}
	requireSeatbelt(t)
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not on PATH")
	}

	work := t.TempDir()
	plan := planFor(t, Spec{Mode: ModeSeatbelt, WorkDir: work})

	command, args := plan.Wrap(bin, []string{"-p", "Reply with exactly: SANDBOX_OK"})
	cmd := exec.Command(command, args...)
	cmd.Dir = work
	// The real env path: inherited set reduced to the allowlist, no overrides.
	cmd.Env = spawnEnvForTest(plan)

	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() {
		out, runErr = cmd.CombinedOutput()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(90 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("claude timed out inside the sandbox")
	}

	if runErr != nil {
		t.Fatalf("claude failed inside the sandbox: %v\n--- output ---\n%s", runErr, out)
	}
	if !strings.Contains(string(out), "SANDBOX_OK") {
		t.Fatalf("unexpected reply from a boxed claude:\n%s", out)
	}
	t.Logf("boxed claude replied: %s", strings.TrimSpace(string(out)))

	// The agent authenticated via Keychain with no credential in its environment.
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			t.Fatal("smoke test forwarded a credential; the Keychain path was not exercised")
		}
	}
}

// spawnEnvForTest mirrors agent.SpawnEnvFor without importing the parent package
// (which would be an import cycle in the other direction for readers).
func spawnEnvForTest(plan *Plan) []string {
	allow := make(map[string]bool)
	for _, name := range plan.EnvAllowlist() {
		allow[name] = true
	}
	var out []string
	for _, kv := range os.Environ() {
		key, _, ok := strings.Cut(kv, "=")
		if ok && allow[key] {
			out = append(out, kv)
		}
	}
	return out
}

// A confined agent must still be able to write inside its workspace the way a
// real coding session does — create files, and create nested directories.
func TestBoxedWorkspaceBehavesLikeAWorkspace(t *testing.T) {
	requireSeatbelt(t)
	work := t.TempDir()
	plan := planFor(t, Spec{Mode: ModeSeatbelt, WorkDir: work})

	script := "mkdir -p " + quote(filepath.Join(work, "a", "b")) +
		" && echo hi > " + quote(filepath.Join(work, "a", "b", "c.txt")) +
		" && cat " + quote(filepath.Join(work, "a", "b", "c.txt"))
	out, err := runBoxed(plan, script)
	if err != nil {
		t.Fatalf("ordinary workspace writes were blocked: %v (%s)", err, out)
	}
	if !strings.Contains(out, "hi") {
		t.Fatalf("unexpected output: %q", out)
	}
}
