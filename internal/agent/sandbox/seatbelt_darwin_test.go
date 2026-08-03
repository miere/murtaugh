package sandbox

// The _darwin filename is a build constraint: these tests compile and run only on
// macOS, which is the only platform seatbelt exists on.
//
// They run the real sandbox-exec and assert the kernel actually enforces what the
// generated profile claims. A profile that merely LOOKS right is worth nothing —
// the failure mode this guards against is a rule that silently never matches (see
// realPath and the symlinked /tmp), which leaves an agent apparently confined and
// actually not.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSeatbeltEnforcesWriteBoundary(t *testing.T) {
	requireSeatbelt(t)
	work := t.TempDir()
	outside := outsideTheBox(t)
	plan := planFor(t, Spec{Mode: ModeSeatbelt, WorkDir: work})

	t.Run("write inside the workdir succeeds", func(t *testing.T) {
		target := filepath.Join(work, "allowed.txt")
		if out, err := runBoxed(plan, "echo ok > "+quote(target)); err != nil {
			t.Fatalf("write to the workdir was blocked: %v (%s)", err, out)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("file was not written: %v", err)
		}
	})

	t.Run("write outside the workdir is denied", func(t *testing.T) {
		target := filepath.Join(outside, "escaped.txt")
		if out, err := runBoxed(plan, "echo pwned > "+quote(target)); err == nil {
			t.Fatalf("write outside the workdir succeeded — the box is not enforcing (%s)", out)
		}
		if _, err := os.Stat(target); err == nil {
			t.Fatal("file outside the workdir was created")
		}
	})

	// The property the whole design rests on: `claude` spawns node, git, ripgrep
	// and the mcp-bridge grandchild, none of which cooperate with the sandbox.
	t.Run("a child process inherits the box", func(t *testing.T) {
		target := filepath.Join(outside, "child.txt")
		if out, err := runBoxed(plan, "/bin/sh -c "+quote("echo pwned > "+target)); err == nil {
			t.Fatalf("a child escaped the box (%s)", out)
		}
	})

	// A descendant must not be able to re-sandbox itself more permissively.
	t.Run("a child cannot loosen the box", func(t *testing.T) {
		target := filepath.Join(outside, "loose.txt")
		script := seatbeltBinary + ` -p "(version 1)(allow default)" /bin/sh -c ` + quote("echo pwned > "+target)
		if out, err := runBoxed(plan, script); err == nil {
			t.Fatalf("a child loosened the box (%s)", out)
		}
	})
}

// The read posture is deliberately asymmetric with the write posture: allow by
// default, minus an explicit deny list. Both halves are asserted here because
// getting either backwards is silent.
func TestSeatbeltReadPosture(t *testing.T) {
	requireSeatbelt(t)
	work := t.TempDir()
	secrets := filepath.Join(work, "secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secrets, "key"), []byte("shh"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	plan := planFor(t, Spec{Mode: ModeSeatbelt, WorkDir: work, DenyRead: []string{secrets}})

	// A path mentioned nowhere in the profile stays readable.
	if out, err := runBoxed(plan, "cat /etc/hosts"); err != nil {
		t.Fatalf("an unmentioned host file was unreadable — read posture is not allow-by-default (%s)", out)
	}
	// ...minus the explicit denials.
	if out, err := runBoxed(plan, "cat "+quote(filepath.Join(secrets, "key"))); err == nil {
		t.Fatalf("a denied path was readable (%s)", out)
	}
}

// Claude Code's session state lives in ~/.claude; without a write carve-out there
// it loses resume history and misbehaves in ways that never mention the sandbox.
func TestSeatbeltAllowsClaudeStateDir(t *testing.T) {
	requireSeatbelt(t)
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory")
	}
	plan := planFor(t, Spec{Mode: ModeSeatbelt, WorkDir: t.TempDir()})

	probe := filepath.Join(home, ".claude", ".murtaugh-sandbox-probe")
	t.Cleanup(func() { _ = os.Remove(probe) })
	if out, err := runBoxed(plan, "mkdir -p "+quote(filepath.Dir(probe))+" && echo ok > "+quote(probe)); err != nil {
		t.Fatalf("~/.claude is not writable inside the box: %v (%s)", err, out)
	}
}

// outsideTheBox returns a directory genuinely outside the always-on writable set.
//
// t.TempDir() is NOT usable here: it allocates under $TMPDIR, which is one of the
// always-on carve-outs (node will not run without a writable temp dir). Using it
// as the "outside" path makes every write-denial assertion below pass vacuously —
// the box is never exercised and the test reports green on a broken profile. The
// package source dir is under the repo, so it is outside every carve-out.
func outsideTheBox(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return wd
}

func requireSeatbelt(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(seatbeltBinary); err != nil {
		t.Skipf("%s unavailable", seatbeltBinary)
	}
}

func planFor(t *testing.T, spec Spec) *Plan {
	t.Helper()
	plan, err := Resolve(spec)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if plan == nil {
		t.Fatal("expected a plan for mode seatbelt")
	}
	return plan
}

// runBoxed executes a shell script inside the plan's sandbox.
func runBoxed(plan *Plan, script string) (string, error) {
	command, args := plan.Wrap("/bin/sh", []string{"-c", script})
	out, err := exec.Command(command, args...).CombinedOutput()
	return string(out), err
}

func quote(s string) string { return "'" + s + "'" }
