package sandbox

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestResolveOffYieldsNoPlan(t *testing.T) {
	for _, mode := range []string{"", ModeOff} {
		plan, err := Resolve(Spec{Mode: mode, WorkDir: t.TempDir()})
		if err != nil {
			t.Fatalf("mode %q: unexpected error: %v", mode, err)
		}
		if plan != nil {
			t.Fatalf("mode %q: expected no plan, got %+v", mode, plan)
		}
	}
}

func TestResolveRejectsUnknownMode(t *testing.T) {
	if _, err := Resolve(Spec{Mode: "bwrap", WorkDir: t.TempDir()}); err == nil {
		t.Fatal("expected an error for an unknown mode")
	}
}

// A nil *Plan must behave as "unconfined" through every method, because that is
// the value the build seam produces for mode off and the backends call these
// methods unconditionally.
func TestNilPlanIsUnconfined(t *testing.T) {
	var plan *Plan

	cmd, args := plan.Wrap("claude", []string{"-p", "--verbose"})
	if cmd != "claude" || strings.Join(args, " ") != "-p --verbose" {
		t.Fatalf("nil plan altered the invocation: %q %v", cmd, args)
	}
	if plan.EnvAllowlist() != nil {
		t.Fatal("nil plan must inherit the whole environment")
	}
	if plan.Describe() != "off" {
		t.Fatalf("nil plan describes itself as %q", plan.Describe())
	}
}

func TestEnvAllowlistIsAdditiveNotReplacing(t *testing.T) {
	got := mergeEnvAllow([]string{"ANTHROPIC_API_KEY", "PATH"})

	for _, want := range defaultEnvAllow {
		if !contains(got, want) {
			t.Fatalf("profile env dropped the built-in %q — allowlist must be additive", want)
		}
	}
	if !contains(got, "ANTHROPIC_API_KEY") {
		t.Fatal("profile env entry was not added")
	}
	// PATH was named in both; it must not appear twice.
	var pathCount int
	for _, name := range got {
		if name == "PATH" {
			pathCount++
		}
	}
	if pathCount != 1 {
		t.Fatalf("PATH appears %d times, want 1", pathCount)
	}
}

// The kernel matches rules against fully resolved paths. On macOS the two
// directories an agent needs most are symlinks (/tmp -> /private/tmp, /var/... ->
// /private/var/...), so a rule written against the unresolved path never matches
// and the denial names a path that looks allowed.
func TestRealPathResolvesSymlinks(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("symlinked /tmp is a macOS layout")
	}
	if got := realPath("/tmp"); got != "/private/tmp" {
		t.Fatalf("realPath(/tmp) = %q, want /private/tmp", got)
	}
}

// A workdir that does not exist yet (or a socket before bind) still has to
// produce a usable rule, via the deepest existing ancestor.
func TestRealPathHandlesMissingLeaf(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "not-created-yet", "deeper")

	got := realPath(missing)

	resolvedBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("resolve base: %v", err)
	}
	want := filepath.Join(resolvedBase, "not-created-yet", "deeper")
	if got != want {
		t.Fatalf("realPath(%q) = %q, want %q", missing, got, want)
	}
}

func TestSeatbeltProfileShape(t *testing.T) {
	work := t.TempDir()
	sock := filepath.Join(t.TempDir(), "bridge.sock")

	profile, err := seatbeltProfile(Spec{Mode: ModeSeatbelt, WorkDir: work, BridgeSocket: sock})
	if err != nil {
		t.Fatalf("build profile: %v", err)
	}

	// Ordering is load-bearing: SBPL takes the LAST matching rule, so a carve-out
	// emitted before the blanket deny is dead text.
	denyAt := strings.Index(profile, "(deny file-write*)")
	allowAt := strings.Index(profile, realPath(work))
	if denyAt < 0 || allowAt < 0 {
		t.Fatalf("profile missing deny or workdir carve-out:\n%s", profile)
	}
	if denyAt > allowAt {
		t.Fatalf("workdir carve-out precedes the blanket write deny — it would be ignored:\n%s", profile)
	}

	// The bridge socket carve-out is what keeps Murtaugh's own tool surface alive.
	if !strings.Contains(profile, realPath(sock)) {
		t.Fatalf("profile omits the bridge socket:\n%s", profile)
	}
	// Credential stores are blinded without configuration.
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		for _, want := range []string{".ssh", ".aws", "gcloud", "gh", ".netrc"} {
			if !strings.Contains(profile, want) {
				t.Fatalf("default deny_read omits %q:\n%s", want, profile)
			}
		}
	}
}

// An explicitly empty (non-nil) deny list must deny nothing — distinct from an
// omitted list, which takes the credential-store defaults.
func TestExplicitEmptyDenyReadDeniesNothing(t *testing.T) {
	profile, err := seatbeltProfile(Spec{Mode: ModeSeatbelt, WorkDir: t.TempDir(), DenyRead: []string{}})
	if err != nil {
		t.Fatalf("build profile: %v", err)
	}
	if strings.Contains(profile, "deny file-read*") {
		t.Fatalf("explicit empty deny_read still emitted read denials:\n%s", profile)
	}
}

func TestSBPLStringEscapes(t *testing.T) {
	if got := sbplString(`/a"b\c`); got != `"/a\"b\\c"` {
		t.Fatalf("sbplString = %s", got)
	}
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
