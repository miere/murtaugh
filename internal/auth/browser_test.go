package auth

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBrowserGuardShadowsTheLaunchers is the property the whole type exists
// for: with the guard on PATH, a bare `open` must resolve INSIDE the guard.
//
// It is asserted through exec.LookPath rather than by running the launcher,
// because a regression here would otherwise be reported by a browser window
// appearing on whoever's machine ran the test.
func TestBrowserGuardShadowsTheLaunchers(t *testing.T) {
	guard, err := NewBrowserGuard()
	if err != nil {
		t.Fatalf("NewBrowserGuard: %v", err)
	}
	t.Cleanup(func() { _ = guard.Close() })

	inherited := []string{"PATH=" + os.Getenv("PATH")}
	overrides := guard.Overrides(inherited)

	guarded := ""
	for _, entry := range overrides {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			guarded = value
		}
	}
	if guarded == "" {
		t.Fatal("Overrides did not set PATH")
	}

	t.Setenv("PATH", guarded)
	for _, name := range launchers {
		resolved, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("LookPath(%q) with the guard on PATH: %v", name, err)
		}
		if filepath.Dir(resolved) != guard.Dir() {
			t.Errorf("LookPath(%q) = %q, want it inside the guard %q", name, resolved, guard.Dir())
		}
	}
}

// TestBrowserGuardKeepsTheInheritedPath. The sign-in still shells out for other
// things — `security`, writing the login keychain, above all. A guard that
// replaced PATH would trade a stray browser window for a credential that cannot
// be persisted, which is the worse of the two by a distance.
func TestBrowserGuardKeepsTheInheritedPath(t *testing.T) {
	guard, err := NewBrowserGuard()
	if err != nil {
		t.Fatalf("NewBrowserGuard: %v", err)
	}
	t.Cleanup(func() { _ = guard.Close() })

	overrides := guard.Overrides([]string{"PATH=/usr/bin:/bin"})
	want := guard.Dir() + string(os.PathListSeparator) + "/usr/bin:/bin"
	if got := overrides[0]; got != "PATH="+want {
		t.Errorf("PATH override = %q, want %q", got, "PATH="+want)
	}
}

// TestBrowserGuardHandlesAnAbsentPath. A child environment that carries no PATH
// at all must not yield a leading separator, which several shells read as "the
// current directory".
func TestBrowserGuardHandlesAnAbsentPath(t *testing.T) {
	guard, err := NewBrowserGuard()
	if err != nil {
		t.Fatalf("NewBrowserGuard: %v", err)
	}
	t.Cleanup(func() { _ = guard.Close() })

	overrides := guard.Overrides(nil)
	if got := overrides[0]; got != "PATH="+guard.Dir() {
		t.Errorf("PATH override = %q, want the guard alone", got)
	}
}

// TestBrowserGuardSetsBrowserForTheLinuxBranch. macOS is handled by PATH, but
// the CLI's non-darwin branch goes through xdg-open and does honour BROWSER.
func TestBrowserGuardSetsBrowserForTheLinuxBranch(t *testing.T) {
	guard, err := NewBrowserGuard()
	if err != nil {
		t.Fatalf("NewBrowserGuard: %v", err)
	}
	t.Cleanup(func() { _ = guard.Close() })

	want := "BROWSER=" + filepath.Join(guard.Dir(), "open")
	found := false
	for _, entry := range guard.Overrides(nil) {
		if entry == want {
			found = true
		}
	}
	if !found {
		t.Errorf("Overrides did not set %q", want)
	}
}

// TestBrowserGuardCloseIsIdempotent, so callers can defer it next to an error
// path that already closed.
func TestBrowserGuardCloseIsIdempotent(t *testing.T) {
	guard, err := NewBrowserGuard()
	if err != nil {
		t.Fatalf("NewBrowserGuard: %v", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	var nilGuard *BrowserGuard
	if err := nilGuard.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
	if got := nilGuard.Overrides(nil); got != nil {
		t.Errorf("nil Overrides = %v, want nil", got)
	}
}
