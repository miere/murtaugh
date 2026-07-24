package acp

import (
	"os"
	"strings"
)

// nestedClaudeMarker is the env var Claude Code sets to mark "you are running
// inside a Claude Code session". A Claude-Code-based ACP adapter (claude-agent-acp)
// refuses to launch when it inherits it — "Claude Code cannot be launched inside
// another Claude Code session". Murtaugh's ACP agents are independent top-level
// processes, not nested sessions; the marker is only inherited by accident when
// the daemon itself happens to run inside a Claude Code session (dev/testing).
// Stripping it breaks that cyclic chain so the agent starts normally; it is
// harmless for non-Claude agents, which ignore the variable.
const nestedClaudeMarker = "CLAUDECODE"

// agentEnv builds the spawn environment for an ACP agent process: the inherited
// environment with the nested-Claude-Code marker removed, then the profile's
// overrides appended (a duplicate key resolves to the override, since exec takes
// the last entry).
func agentEnv(extra []string) []string {
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
