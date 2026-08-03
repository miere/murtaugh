package agent

import (
	"os"
	"strings"
)

// nestedClaudeMarker is the env var Claude Code sets to mark "you are running
// inside a Claude Code session". A Claude-Code-based agent — the `claude` CLI
// (claude_code) or a claude-agent-acp adapter — refuses to launch when it
// inherits it ("Claude Code cannot be launched inside another Claude Code
// session"). Murtaugh's agents are independent top-level processes, not nested
// sessions; the marker is only inherited by accident when the daemon itself runs
// inside a Claude Code session (dev/testing). Stripping it breaks that cyclic
// chain, and it is harmless for non-Claude agents, which ignore it.
//
// A sandboxed agent gets this for free — the marker is not on the inherited-env
// allowlist, along with the rest of the CLAUDE_CODE_* family — but the strip
// stays unconditional so the unsandboxed path keeps working.
const nestedClaudeMarker = "CLAUDECODE"

// Sandbox confines a spawned agent process. It is implemented by *sandbox.Plan;
// the interface lives here so the backends (acp, claudecode) depend only on the
// package they already import.
//
// A nil Sandbox means unconfined and takes exactly the pre-sandbox path. Beware
// the typed-nil trap: assign a nil *sandbox.Plan to this interface only through
// an explicit nil check, never by direct assignment.
type Sandbox interface {
	// Wrap rewrites (command, args) into the confined invocation.
	Wrap(command string, args []string) (string, []string)
	// EnvAllowlist names the inherited environment variables to keep. nil keeps
	// everything.
	EnvAllowlist() []string
}

// WrapCommand rewrites an invocation into its confined form, or returns it
// untouched when sb is nil. Backends call this at the exec site so the nil check
// lives in one place rather than a copy per backend.
func WrapCommand(sb Sandbox, command string, args []string) (string, []string) {
	if sb == nil {
		return command, args
	}
	return sb.Wrap(command, args)
}

// SpawnEnv builds the environment for a spawned agent process: the inherited
// environment with the nested-Claude-Code marker removed, then extra overrides
// appended (a duplicate key resolves to the override — exec takes the last entry).
// Every agent backend that spawns a child process uses it (or SpawnEnvFor), so
// the guard lives in one place rather than a copy per backend.
func SpawnEnv(extra []string) []string { return spawnEnv(nil, extra) }

// SpawnEnvFor builds the environment for a process spawned under sb. A nil sb is
// identical to SpawnEnv; otherwise the inherited environment is first reduced to
// the sandbox's allowlist.
//
// The layering is the point: the allowlist filters what is INHERITED, while extra
// (the profile's own `env:` map) is applied afterwards and unconditionally. That
// keeps the two concerns separate — an operator who needs to inject an API key
// puts it in the profile and never has to widen the allowlist to get it through.
func SpawnEnvFor(sb Sandbox, extra []string) []string {
	var allow []string
	if sb != nil {
		allow = sb.EnvAllowlist()
	}
	return spawnEnv(allow, extra)
}

// spawnEnv filters the inherited environment and appends the overrides. A nil
// allow slice inherits everything but the nested-Claude marker; a non-nil one
// keeps only the named variables.
func spawnEnv(allow []string, extra []string) []string {
	var allowed map[string]bool
	if allow != nil {
		allowed = make(map[string]bool, len(allow))
		for _, name := range allow {
			if name = strings.TrimSpace(name); name != "" {
				allowed[name] = true
			}
		}
	}

	base := os.Environ()
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || key == nestedClaudeMarker {
			continue
		}
		if allowed != nil && !allowed[key] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}
