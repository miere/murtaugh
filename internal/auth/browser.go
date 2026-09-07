package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BrowserGuard keeps an authentication CLI from opening a browser on the HOST
// machine.
//
// It exists because `claude auth login` has no `--no-launch-browser` switch —
// unlike the gcloud profiles, which pass one — and opens the consent page
// itself. On the daemon that is always the wrong machine: Murtaugh runs under
// launchd inside the admin's GUI session, so the window lands on the host
// desktop while the admin, who may be nowhere near it, is waiting on a Slack
// card. The sign-in then has two half-drivers and completes under neither.
//
// Suppression works through PATH rather than an environment variable, and the
// distinction is load-bearing. On macOS the CLI bundles the `open` npm package,
// whose darwin branch spawns the BARE command `open` and resolves it through
// PATH; it consults no BROWSER variable on that path, so a BROWSER override
// alone changes nothing. A directory of no-op stand-ins placed FIRST on PATH is
// what actually intercepts the launch. BROWSER is set as well, for the Linux
// branch, which shells out to xdg-open and does honour it.
//
// The stand-ins exit 0 — they report success. A launcher that failed would send
// the CLI down its "couldn't open your browser" path, which prints extra prose
// and on some flows an extra prompt; pretending the browser opened preserves the
// output shape the profile's URL pattern was written against.
//
// Only the launchers are shadowed. Everything else the CLI reaches for — most
// importantly `security`, which writes the login keychain — still resolves out
// of the inherited PATH, so a suppressed browser cannot cost us a persisted
// credential.
type BrowserGuard struct {
	dir string
}

// launchers are the browser-opening binaries an auth CLI may reach for. Both
// names are shadowed regardless of platform: the cost is one extra file, and a
// CLI that picks the "wrong" one for the host is exactly the surprise this type
// exists to absorb.
var launchers = []string{"open", "xdg-open"}

// guardScript is the stand-in. It is deliberately inert — no logging, no
// output — because anything it printed would land in the child's stream and be
// read by the profile's URL matcher.
const guardScript = "#!/bin/sh\n" +
	"# Murtaugh: browser launch suppressed. The sign-in link is delivered in Slack.\n" +
	"exit 0\n"

// NewBrowserGuard materialises the stand-ins in a private temporary directory.
// The caller must Close it once the child has exited.
func NewBrowserGuard() (*BrowserGuard, error) {
	dir, err := os.MkdirTemp("", "murtaugh-authguard-")
	if err != nil {
		return nil, fmt.Errorf("auth: browser guard: create directory: %w", err)
	}
	for _, name := range launchers {
		// 0o700: the guard has to be executable to shadow anything, and nothing
		// outside this process needs to read it.
		if err := os.WriteFile(filepath.Join(dir, name), []byte(guardScript), 0o700); err != nil {
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("auth: browser guard: write %s: %w", name, err)
		}
	}
	return &BrowserGuard{dir: dir}, nil
}

// Dir is the directory holding the stand-ins.
func (g *BrowserGuard) Dir() string {
	if g == nil {
		return ""
	}
	return g.dir
}

// Overrides returns the environment entries that route the child's browser
// launch into the guard, expressed against the environment the child would
// otherwise have run with.
//
// It takes that environment rather than reading os.Environ itself because the
// caller may already be layering the requesting agent's own overrides on top,
// and a PATH prepended to the daemon's copy would silently discard theirs.
func (g *BrowserGuard) Overrides(inherited []string) []string {
	if g == nil {
		return nil
	}
	path := g.dir
	if existing := lookupEnv(inherited, "PATH"); existing != "" {
		path = g.dir + string(os.PathListSeparator) + existing
	}
	return []string{
		"PATH=" + path,
		"BROWSER=" + filepath.Join(g.dir, "open"),
	}
}

// Close removes the stand-ins. Safe on a nil guard so callers can defer it
// unconditionally.
func (g *BrowserGuard) Close() error {
	if g == nil || g.dir == "" {
		return nil
	}
	return os.RemoveAll(g.dir)
}

// lookupEnv reads one variable out of a KEY=VALUE list. A duplicate key
// resolves last-wins, matching how the platform reads the same list.
func lookupEnv(env []string, key string) string {
	prefix := key + "="
	value := ""
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			value = strings.TrimPrefix(entry, prefix)
		}
	}
	return value
}
