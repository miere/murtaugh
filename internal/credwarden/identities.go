package credwarden

import (
	"strings"

	"github.com/miere/murtaugh/internal/config"
)

// ClaudeCodeIdentities is derived from the agents rather than configured, so a
// credential is watched exactly while some claude_code agent runs on it.
func ClaudeCodeIdentities(agents map[string]config.AgentProfile) []Identity {
	var out []Identity
	for _, profile := range agents {
		if profile.ResolvedKind() != config.AgentKindClaudeCode || profile.ClaudeCode == nil {
			continue
		}
		command := strings.TrimSpace(profile.ClaudeCode.Command)
		if command == "" {
			continue
		}
		out = append(out, Identity{Command: command, Home: homeOverride(profile)})
	}
	return out
}

func homeOverride(profile config.AgentProfile) string {
	for _, kv := range profile.EnvOverrides() {
		if key, value, ok := strings.Cut(kv, "="); ok && key == "HOME" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
