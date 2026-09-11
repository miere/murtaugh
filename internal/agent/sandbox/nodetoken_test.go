package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tokenFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "node-token")
	if err := os.WriteFile(path, []byte("mrtg_node_0123456789abcdef_secret\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return path
}

func denyLineFor(path string) string {
	return "(deny file-read* " + subpathAndLiteral(realPath(path)) + ")"
}

func denyWriteLineFor(path string) string {
	return "(deny file-write* " + subpathAndLiteral(realPath(path)) + ")"
}

// The default token path sits inside the always-writable workdir, so denying
// reads alone would still let an agent delete or replace it.
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

// A profile's deny_read replaces the defaults, so `deny_read: []` would drop the
// token if its rule lived in that list.
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

	profile, err := seatbeltProfile(Spec{Mode: ModeSeatbelt, WorkDir: t.TempDir(), NodeTokenPath: token, DenyRead: []string{}})
	if err != nil {
		t.Fatalf("seatbeltProfile: %v", err)
	}
	if strings.Contains(profile, denyLineFor("~/.ssh")) {
		t.Fatal("an explicitly empty deny_read no longer drops the defaults; the premise of this test has changed")
	}
}

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

func TestProfileUsesTheResolvedTokenPath(t *testing.T) {
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

// realPath("") resolves to the cwd, which for an agent is its workspace, so a
// rule for an empty token path would deny the agent its own workdir.
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

func nonBlank(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}
