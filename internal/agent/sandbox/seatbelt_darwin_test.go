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
	"strings"
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

// TestSeatbeltDeniesTheNodeToken is the empirical half of the node-credential
// claim: the profile-text assertions in nodetoken_test.go prove the rule is
// EMITTED, and this proves the kernel acts on it.
//
// Both halves are needed. A rule that reads correctly and never matches is this
// package's signature failure (see realPath), so "the profile denies it" is not
// the same statement as "the agent cannot read it", and only this test makes the
// second one.
func TestSeatbeltDeniesTheNodeToken(t *testing.T) {
	requireSeatbelt(t)
	work := t.TempDir()

	// The credential lives outside the workdir here only to keep this test to one
	// claim. The location is NOT what protects it — see
	// TestSeatbeltDeniesTheNodeTokenInsideTheWorkdir for the case
	// nodetoken.PathFor actually produces.
	token := filepath.Join(outsideTheBox(t), ".murtaugh-node-token-probe")
	if err := os.WriteFile(token, []byte("mrtg_node_0123456789abcdef_secret\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(token) })

	// The case that catches the real bug. An explicitly empty deny_read is a
	// legal profile that denies nothing (config.SandboxConfig says so), so if the
	// token rule were merged into that list this subtest would read the
	// credential — while the nil-list subtest passed and hid it.
	for name, denyRead := range map[string][]string{
		"with the default deny list": nil,
		"with deny_read: []":         {},
	} {
		t.Run(name, func(t *testing.T) {
			plan := planFor(t, Spec{Mode: ModeSeatbelt, WorkDir: work, NodeTokenPath: token, DenyRead: denyRead})
			requireBoxApplies(t, plan)

			// Read posture first: an unmentioned file must still be readable, or
			// the denial below would pass for the wrong reason (a box that reads
			// nothing at all).
			if out, err := runBoxed(plan, "cat /etc/hosts"); err != nil {
				t.Fatalf("an unmentioned host file was unreadable — the denial below would prove nothing (%s)", out)
			}
			if out, err := runBoxed(plan, "cat "+quote(token)); err == nil {
				t.Fatalf("the boxed agent read its node's credential (%s)", out)
			}
		})
	}
}

// TestSeatbeltDeniesTheNodeTokenInsideTheWorkdir is the shape a default install
// actually has: nodetoken.PathFor puts the credential in the config directory,
// and an agent with no `workdir:` of its own is resolved onto that same
// directory. The token is therefore a direct child of the workdir, which is in
// the always-on WRITE carve-out — so the read deny alone would leave a boxed
// agent able to delete or replace the credential it cannot read.
func TestSeatbeltDeniesTheNodeTokenInsideTheWorkdir(t *testing.T) {
	requireSeatbelt(t)
	work := t.TempDir()
	token := filepath.Join(work, "node-token")
	if err := os.WriteFile(token, []byte("mrtg_node_0123456789abcdef_secret\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	plan := planFor(t, Spec{Mode: ModeSeatbelt, WorkDir: work, NodeTokenPath: token})
	requireBoxApplies(t, plan)

	// The control, and it is the whole point of this test: the workdir IS
	// writable, so every denial below is the token's own rule and not the box
	// refusing writes everywhere.
	sibling := filepath.Join(work, "scratch")
	if out, err := runBoxed(plan, "echo ok > "+quote(sibling)); err != nil {
		t.Fatalf("the workdir was not writable — the denials below would prove nothing (%s)", out)
	}

	if out, err := runBoxed(plan, "cat "+quote(token)); err == nil {
		t.Fatalf("the boxed agent read the credential from its own workdir (%s)", out)
	}
	if out, err := runBoxed(plan, "echo tampered > "+quote(token)); err == nil {
		t.Fatalf("the boxed agent rewrote its node's credential (%s)", out)
	}
	if out, err := runBoxed(plan, "rm -f "+quote(token)); err == nil {
		t.Fatalf("the boxed agent deleted its node's credential (%s)", out)
	}
	if _, err := os.ReadFile(token); err != nil {
		t.Fatalf("the credential did not survive the box: %v", err)
	}
}

// requireBoxApplies skips when the kernel refuses to apply ANY profile.
//
// That happens when the test process is already confined — an agent harness, or
// a CI runner inside a sandbox — and it is not a property of the code under
// test: even `(version 1)(allow default)` fails with the same EPERM. Skipping is
// the honest outcome, because a test that cannot enter the box cannot report
// anything about what the box denies.
func requireBoxApplies(t *testing.T, plan *Plan) {
	t.Helper()
	if out, err := runBoxed(plan, "true"); err != nil && strings.Contains(out, "sandbox_apply") {
		t.Skipf("sandbox-exec cannot apply a profile in this environment (%s); the kernel-level claim cannot be checked here", strings.TrimSpace(out))
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
