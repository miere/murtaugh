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
const nestedClaudeMarker = "CLAUDECODE"

// SpawnEnv builds the environment for a spawned agent process: the inherited
// environment with the nested-Claude-Code marker removed, then extra overrides
// appended (a duplicate key resolves to the override — exec takes the last entry).
// Every agent backend that spawns a child process uses it, so the guard lives in
// one place rather than a copy per backend.
func SpawnEnv(extra []string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		if strings.HasPrefix(kv, nestedClaudeMarker+"=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}
