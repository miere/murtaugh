package gateway

import (
	"context"
	"strings"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/credwarden"
)

// claudeCodeIdentities derives the set of Claude Code credentials this daemon is
// responsible for keeping fresh.
//
// It is DERIVED, never configured. There is no enable flag: the warden exists
// because a claude_code agent exists, and it disappears when the last one is
// removed. An operator cannot forget to turn it on, and — because it is not a
// config-store job — no agent can enumerate, run, redefine, or silently disable
// it.
//
// Only `claude_code` agents count. An ACP agent may well be a Claude Code
// process behind an adapter, but the adapter's command is arbitrary and
// unrecognisable, so guessing would be a heuristic that fails quietly. Per the
// operator's decision, credentials for ACP agents are the admin's own
// responsibility; claude_code is the supported path for Claude users.
//
// The result is keyed by (command, HOME) rather than by agent, because that pair
// — not the profile — is what identifies a credential store. N profiles sharing
// one credential must produce ONE watcher: concurrent refreshes race the
// server's rotation of the refresh token, which is the very failure the warden
// exists to prevent. credwarden.New collapses the duplicates.
func claudeCodeIdentities(agents map[string]config.AgentProfile) []credwarden.Identity {
	var out []credwarden.Identity
	for _, profile := range agents {
		if profile.ResolvedKind() != config.AgentKindClaudeCode || profile.ClaudeCode == nil {
			continue
		}
		command := strings.TrimSpace(profile.ClaudeCode.Command)
		if command == "" {
			continue
		}
		out = append(out, credwarden.Identity{
			Command: command,
			Home:    homeOverride(profile),
		})
	}
	return out
}

// homeOverride returns the HOME this profile's agent actually runs with, or ""
// when it inherits the daemon's.
//
// A profile's `env:` map is layered onto the spawn environment unconditionally
// (agent.SpawnEnvFor), so it CAN redirect HOME and thereby point at a different
// credential file. Reading it through EnvOverrides is what keeps that agent from
// being watched under the wrong identity — or, worse, silently sharing a watcher
// with an agent whose credential lives somewhere else.
func homeOverride(profile config.AgentProfile) string {
	for _, kv := range profile.EnvOverrides() {
		if key, value, ok := strings.Cut(kv, "="); ok && key == "HOME" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// startCredWarden runs the credential warden for this daemon's lifetime. A nil
// warden (no claude_code agents) is a no-op, mirroring the other optional
// background loops.
func (a *Gateway) startCredWarden(ctx context.Context) {
	if a.credWarden == nil {
		return
	}
	go a.credWarden.Run(ctx)
}
