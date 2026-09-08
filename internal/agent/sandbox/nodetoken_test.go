package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests are about the POLICY TEXT the generator emits, which is checkable
// on any platform. Whether the kernel then enforces it is a different claim and
// is asserted separately, against the real sandbox, in
// TestSeatbeltDeniesTheNodeToken (seatbelt_darwin_test.go).
//
// The split matters: a rule that reads correctly and never matches is the
// failure mode this package's doc singles out, so neither test is sufficient
// alone.

// tokenFile writes a credential file outside any of the always-on carve-outs and
// returns its path.
func tokenFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "node-token")
	if err := os.WriteFile(path, []byte("mrtg_node_0123456789abcdef_secret\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return path
}

// denyLineFor renders the rule the profile must contain for a path, resolved the
// way the kernel resolves it. Building it from realPath rather than from the
// literal path is the point: on macOS a temp dir is /var/folders/… which is a
// symlink to /private/var/folders/…, and a rule written against the unresolved
// form never matches anything.
func denyLineFor(path string) string {
	return "(deny file-read* " + subpathAndLiteral(realPath(path)) + ")"
}

// denyWriteLineFor is the same for the write half of the rule.
func denyWriteLineFor(path string) string {
	return "(deny file-write* " + subpathAndLiteral(realPath(path)) + ")"
}

// TestProfileDeniesWritingTheNodeTokenInsideTheWorkdir is the case the file's
// LOCATION was once claimed to handle on its own. nodetoken.PathFor puts the
// credential in the config directory, and an agent with no `workdir:` of its own
// resolves onto that same directory — so the token is routinely a direct child
// of the workdir, which writablePaths put in the always-on write carve-out. Read
// denial alone would still let a boxed agent delete or replace the credential.
func TestProfileDeniesWritingTheNodeTokenInsideTheWorkdir(t *testing.T) {
	work := t.TempDir()
	token := filepath.Join(work, "node-token")
	if err := os.WriteFile(token, []byte("mrtg_node_0123456789abcdef_secret\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	profile, err := seatbeltProfile(Spec{Mode: ModeSeatbelt, WorkDir: work, NodeTokenPath: token})
	if err != nil {
		t.Fatalf("seatbeltProfile: %v", err)
	}

	// The premise: the workdir really is carved out for writing, so a read-only
	// rule would leave the credential rewritable.
	carveOut := strings.Index(profile, "(allow file-write* "+subpathAndLiteral(realPath(work)))
	if carveOut < 0 {
		t.Fatalf("the workdir is not in the write carve-out; the premise of this test has changed:\n%s", profile)
	}
	denyAt := strings.Index(profile, denyWriteLineFor(token))
	if denyAt < 0 {
		t.Fatalf("the profile does not deny WRITING the node token.\nwant a line: %s\ngot:\n%s", denyWriteLineFor(token), profile)
	}
	if denyAt < carveOut {
		t.Fatal("the write deny is emitted before the workdir carve-out; SBPL's last matching rule wins, so the carve-out would re-open the credential")
	}
}

// TestProfileDeniesTheNodeTokenEvenWithAnEmptyDenyList is the trap the whole
// unconditional-emission design exists for. Spec.DenyRead REPLACES the credential
// defaults rather than adding to them (config.SandboxConfig says so out loud:
// "an explicitly empty list denies nothing"), so an agent whose profile sets
// `deny_read: []` would take its node's credential out of the deny list with it
// if the token rule were merged into that list.
func TestProfileDeniesTheNodeTokenEvenWithAnEmptyDenyList(t *testing.T) {
	token := tokenFile(t)
	want := denyLineFor(token)

	for name, denyRead := range map[string][]string{
		"nil deny list (the credential defaults apply)": nil,
		"explicitly empty deny list":                    {},
		"a deny list naming something else":             {"/etc/shadow"},
	} {
		t.Run(name, func(t *testing.T) {
			profile, err := seatbeltProfile(Spec{Mode: ModeSeatbelt, WorkDir: t.TempDir(), NodeTokenPath: token, DenyRead: denyRead})
			if err != nil {
				t.Fatalf("seatbeltProfile: %v", err)
			}
			if !strings.Contains(profile, want) {
				t.Fatalf("the profile does not deny the node token.\nwant a line: %s\ngot:\n%s", want, profile)
			}
		})
	}

	// And the documented consequence of that replacement, pinned here so the two
	// halves of the argument sit together: an empty list really does drop the
	// credential-store defaults, which is why the token rule could not live in it.
	profile, err := seatbeltProfile(Spec{Mode: ModeSeatbelt, WorkDir: t.TempDir(), NodeTokenPath: token, DenyRead: []string{}})
	if err != nil {
		t.Fatalf("seatbeltProfile: %v", err)
	}
	if strings.Contains(profile, denyLineFor("~/.ssh")) {
		t.Fatal("an explicitly empty deny_read no longer drops the defaults; the premise of this test has changed")
	}
}

// TestProfileDeniesTheNodeTokenLast pins the ordering SBPL's last-match-wins
// makes load-bearing: a read carve-out added above must not be able to re-open
// the credential.
func TestProfileDeniesTheNodeTokenLast(t *testing.T) {
	token := tokenFile(t)
	other := tokenFile(t)

	profile, err := seatbeltProfile(Spec{Mode: ModeSeatbelt, WorkDir: t.TempDir(), NodeTokenPath: token, DenyRead: []string{other}})
	if err != nil {
		t.Fatalf("seatbeltProfile: %v", err)
	}
	tokenAt := strings.Index(profile, denyLineFor(token))
	otherAt := strings.Index(profile, denyLineFor(other))
	if tokenAt < 0 || otherAt < 0 {
		t.Fatalf("expected both deny rules in:\n%s", profile)
	}
	if tokenAt < otherAt {
		t.Fatal("the node-token deny is emitted before the profile's own rules; SBPL's last matching rule wins, so a later allow could re-open it")
	}
}

// TestProfileUsesTheResolvedTokenPath states the mistake this package's doc calls
// the single most common way a hand-written seatbelt profile silently fails: the
// kernel matches fully resolved paths, and on macOS a temp dir reaches the token
// through a symlink.
func TestProfileUsesTheResolvedTokenPath(t *testing.T) {
	// A symlink is built here rather than relied upon from $TMPDIR, so the
	// assertion holds on any host: /tmp and /var/folders happen to be symlinks
	// on macOS, but a test that depended on that would quietly skip elsewhere.
	real := tokenFile(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(filepath.Dir(real), link); err != nil {
		t.Skipf("cannot create a symlink on this host: %v", err)
	}
	token := filepath.Join(link, filepath.Base(real))

	resolved := realPath(token)
	if resolved == token {
		t.Fatalf("realPath did not resolve %q; the premise of this test has changed", token)
	}

	profile, err := seatbeltProfile(Spec{Mode: ModeSeatbelt, WorkDir: t.TempDir(), NodeTokenPath: token})
	if err != nil {
		t.Fatalf("seatbeltProfile: %v", err)
	}
	if !strings.Contains(profile, sbplString(resolved)) {
		t.Fatalf("the profile does not name the resolved path %q:\n%s", resolved, profile)
	}
	if strings.Contains(profile, "(literal "+sbplString(token)+")") {
		t.Fatalf("the profile names the UNRESOLVED path %q, which the kernel never matches", token)
	}
}

// TestProfileEmitsNoTokenRuleWithoutAPath: a process that is not a node has no
// credential to hide, and a rule denying "" would deny the working directory —
// realPath("") resolves to the process's own cwd, which for an agent IS its
// workspace.
//
// Counted rather than string-matched, so the assertion cannot be satisfied by a
// comment: with no token path the profile must carry exactly the deny rules the
// deny list asked for and not one more.
func TestProfileEmitsNoTokenRuleWithoutAPath(t *testing.T) {
	for name, denyRead := range map[string][]string{
		"nil deny list":             nil,
		"explicitly empty":          {},
		"a one-entry deny list":     {"/etc/shadow"},
		"a two-entry deny list":     {"/etc/shadow", "/etc/passwd"},
		"blank entries are dropped": {"  "},
	} {
		t.Run(name, func(t *testing.T) {
			profile, err := seatbeltProfile(Spec{Mode: ModeSeatbelt, WorkDir: t.TempDir(), DenyRead: denyRead})
			if err != nil {
				t.Fatalf("seatbeltProfile: %v", err)
			}
			want := len(nonBlank(denyRead))
			if denyRead == nil {
				want = len(nonBlank(defaultDenyRead))
			}
			if got := strings.Count(profile, "(deny file-read*"); got != want {
				t.Fatalf("profile has %d read denials, want %d (no token path was given):\n%s", got, want, profile)
			}
		})
	}
}

// nonBlank counts the entries seatbeltProfile actually emits a rule for.
func nonBlank(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}
