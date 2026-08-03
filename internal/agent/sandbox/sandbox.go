// Package sandbox confines a spawned agent process to an OS-enforced box.
//
// The only implemented backend is macOS seatbelt (`sandbox-exec`), which applies
// a kernel policy (TrustedBSD MAC) and then execs the command. Two properties of
// seatbelt are what the whole design rests on:
//
//   - Confinement is INHERITED by every descendant. `claude` spawns node, git,
//     ripgrep and Murtaugh's own `murtaugh mcp-bridge` grandchild; all of them are
//     boxed without cooperating.
//   - Confinement cannot be LOOSENED by a child. A descendant calling
//     sandbox-exec with a permissive profile gets EPERM.
//
// The posture is deliberately asymmetric: reads are allow-by-default minus an
// explicit deny list (a read allowlist breaks a real agent in a dozen small ways),
// while writes are deny-by-default plus a narrow carve-out. Write confinement is
// the half that actually prevents damage — a boxed agent cannot touch your other
// repos, your dotfiles, your shell rc, or Murtaugh's own config.
//
// Resolution happens exactly once, at the agentbuild seam where the profile and
// the runtime workspace first co-exist; downstream code consumes the *Plan.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Mode selects the confinement backend.
const (
	// ModeOff spawns the agent unconfined — the pre-sandbox behaviour.
	ModeOff = "off"
	// ModeSeatbelt confines the agent with macOS sandbox-exec. Darwin only.
	ModeSeatbelt = "seatbelt"
)

// seatbeltBinary is the macOS sandbox tool. It has been formally deprecated by
// Apple for years but remains the mechanism Chrome and Claude Code's own sandbox
// use; there is no supported replacement for out-of-process confinement.
const seatbeltBinary = "/usr/bin/sandbox-exec"

// defaultEnvAllow is the inherited environment an agent keeps under confinement.
// Everything else is dropped.
//
// Note what is NOT here: no credential variable. Claude Code on macOS
// authenticates through the login Keychain over Mach IPC to securityd, which runs
// outside the box and performs the actual file I/O — so both reads and token
// refresh work with no secret forwarded. An agent that needs API-key auth instead
// should carry it via the backend's own `env:` map, which layers on top of this
// filter unconditionally (see agent.SpawnEnvFor).
//
// TMPDIR earns its place: on macOS it is a per-user /var/folders/... path, not
// /tmp. Dropping it makes node silently fall back to /tmp while the write
// carve-out was computed for the other one — a permission failure deep inside
// node that points nowhere near the sandbox. The env value and the profile rule
// must agree, so they are derived together.
var defaultEnvAllow = []string{
	"PATH",   // find node, git, ripgrep
	"HOME",   // find ~/.claude
	"TMPDIR", // must match the write carve-out below
	"USER",   // git authorship fallback
	"LANG",   // without it tools fall back to ASCII and mangle UTF-8
	"SHELL",  // dropping it silently downgrades shell-outs to /bin/sh
}

// defaultDenyRead are the credential stores an agent is blinded to unless the
// profile overrides the list. These are the paths whose contents are directly
// re-usable as someone else's identity.
var defaultDenyRead = []string{
	"~/.ssh",
	"~/.aws",
	"~/.config/gcloud",
	"~/.config/gh",
	"~/.netrc",
}

// Spec is the resolved, platform-neutral description of the box to build. It is
// assembled at the agentbuild seam from the agent profile plus the runtime facts
// the profile cannot know (the resolved workdir, the MCP bridge socket).
type Spec struct {
	// Mode is ModeOff or ModeSeatbelt. Empty means ModeOff.
	Mode string
	// WorkDir is the agent's resolved workspace — the primary writable surface.
	// Empty is allowed (an agent with no workspace); the box is still built, just
	// with no workspace carve-out.
	WorkDir string
	// Write lists extra writable paths beyond the always-on set. Optional.
	Write []string
	// DenyRead lists paths to blind the agent to. nil takes defaultDenyRead; an
	// explicitly empty (non-nil) slice denies nothing.
	DenyRead []string
	// EnvAllow lists extra inherited environment variables to keep, ADDED to
	// defaultEnvAllow rather than replacing it. Replacing would let a profile that
	// names only ANTHROPIC_API_KEY drop PATH and kill the agent in a way that looks
	// nothing like a config error.
	EnvAllow []string
	// BridgeSocket is the MCP bridge's unix socket. The `murtaugh mcp-bridge`
	// grandchild dials it to serve Murtaugh's own tools back to the agent; connect
	// counts as a write, so it needs an explicit carve-out. Empty when the agent
	// has no bridge (CLI/delegate paths).
	BridgeSocket string
}

// Plan is a resolved, ready-to-apply confinement. A nil *Plan means unconfined;
// callers must convert nil to a nil interface value rather than storing a typed
// nil (see agentbuild.Client).
type Plan struct {
	// prefix is the wrapper argv, e.g. ["/usr/bin/sandbox-exec", "-p", "<profile>"].
	prefix []string
	// envAllow is the inherited-env allowlist.
	envAllow []string
	// profile is retained for diagnostics (troubleshoot bundle, tests).
	profile string
}

// Resolve validates a Spec and builds the Plan. It returns (nil, nil) for
// ModeOff.
//
// Every failure is FAIL CLOSED: an unsupported platform or a missing
// sandbox-exec returns an error rather than degrading to an unconfined spawn.
// Silently losing a security boundary is worse than an agent that refuses to
// start, and a boundary you cannot verify is not a boundary.
func Resolve(spec Spec) (*Plan, error) {
	mode := strings.TrimSpace(spec.Mode)
	if mode == "" {
		mode = ModeOff
	}
	switch mode {
	case ModeOff:
		return nil, nil
	case ModeSeatbelt:
	default:
		return nil, fmt.Errorf("sandbox: unknown mode %q (want %q or %q)", mode, ModeOff, ModeSeatbelt)
	}

	if runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("sandbox: mode %q requires macOS (running on %s)", ModeSeatbelt, runtime.GOOS)
	}
	if _, err := os.Stat(seatbeltBinary); err != nil {
		return nil, fmt.Errorf("sandbox: %s is unavailable: %w", seatbeltBinary, err)
	}

	profile, err := seatbeltProfile(spec)
	if err != nil {
		return nil, err
	}
	return &Plan{
		prefix:   []string{seatbeltBinary, "-p", profile},
		envAllow: mergeEnvAllow(spec.EnvAllow),
		profile:  profile,
	}, nil
}

// Wrap rewrites an invocation into its confined form. A nil Plan returns the
// invocation untouched, so an unsandboxed agent takes exactly the pre-sandbox
// path.
//
// Wrapping happens at the exec site, AFTER each backend has finished layering its
// own arguments — never at the build seam. claudecode.New falls back to
// defaultArgs when Args is empty and appends --model to it; pre-wrapping would
// make Args non-empty and silently eat the entire stream-json launch.
func (p *Plan) Wrap(command string, args []string) (string, []string) {
	if p == nil {
		return command, args
	}
	wrapped := make([]string, 0, len(p.prefix)+len(args))
	wrapped = append(wrapped, p.prefix[1:]...)
	wrapped = append(wrapped, command)
	wrapped = append(wrapped, args...)
	return p.prefix[0], wrapped
}

// EnvAllowlist returns the inherited environment variables to keep. A nil Plan
// returns nil, which agent.SpawnEnvFor reads as "inherit everything".
func (p *Plan) EnvAllowlist() []string {
	if p == nil {
		return nil
	}
	return p.envAllow
}

// Profile returns the generated seatbelt policy, for diagnostics and tests. A nil
// Plan returns "".
func (p *Plan) Profile() string {
	if p == nil {
		return ""
	}
	return p.profile
}

// Describe renders a one-line posture summary for the startup routing log and the
// troubleshoot bundle, so an operator can see at a glance whether an agent is
// boxed. A nil Plan describes itself as off.
func (p *Plan) Describe() string {
	if p == nil {
		return "off"
	}
	return fmt.Sprintf("seatbelt (%d env vars inherited)", len(p.envAllow))
}

// mergeEnvAllow returns defaultEnvAllow plus extra, de-duplicated. Additive by
// design — see Spec.EnvAllow.
func mergeEnvAllow(extra []string) []string {
	seen := make(map[string]bool, len(defaultEnvAllow)+len(extra))
	out := make([]string, 0, len(defaultEnvAllow)+len(extra))
	for _, name := range append(append([]string{}, defaultEnvAllow...), extra...) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// ErrNoWritableSurface is returned when a seatbelt box would have no writable
// path at all — always a misconfiguration, since the agent could not even write
// its own session state.
var ErrNoWritableSurface = errors.New("sandbox: no writable surface")

// expandHome replaces a leading ~ with the user's home directory. A path that
// does not start with ~ is returned unchanged.
func expandHome(p string) string {
	p = strings.TrimSpace(p)
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~/"))
}

// realPath resolves a path to the form seatbelt matches against.
//
// This is the single most common way a hand-written seatbelt profile silently
// fails: the kernel evaluates rules against FULLY RESOLVED paths, and on macOS
// the two directories an agent needs most are symlinks — /tmp -> /private/tmp and
// /var/folders/... -> /private/var/folders/.... A rule written against the
// unresolved path simply never matches, and the resulting denial names a path
// that appears to be allowed.
//
// A path that does not exist yet (a workdir about to be created, the bridge
// socket before bind) cannot be resolved directly, so we resolve the deepest
// existing ancestor and re-append the remainder.
func realPath(p string) string {
	p = filepath.Clean(expandHome(p))
	if !filepath.IsAbs(p) {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	rest := ""
	cur := p
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		if rest == "" {
			rest = filepath.Base(cur)
		} else {
			rest = filepath.Join(filepath.Base(cur), rest)
		}
		cur = parent
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest)
		}
	}
}
