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

// StartBackground starts the work that must run for the DAEMON's lifetime,
// independent of leadership.
//
// The credential warden belongs here rather than in StartServing, and the
// distinction is not bookkeeping. A Claude Code credential is scoped to the
// MACHINE — one keychain item per user, shared by every claude_code agent on the
// host — while leadership is scoped to the cluster. Tying the warden to
// leadership means a standby node lets its own credential rot, so the failover
// that promotes it hands service to a node that cannot authenticate. That is the
// opposite of what a standby is for.
//
// It is also how a real lockout happened: the daemon stood down at 08:10 and the
// warden stopped with it, leaving the credential unattended for twenty-five
// hours. Nothing about a node's leadership makes its credential need refreshing
// any less.
//
// Idempotent: a second call while already running is ignored. A nil warden (no
// claude_code agent configured) is a no-op.
func (a *Gateway) StartBackground(ctx context.Context) {
	if a == nil || a.credWarden == nil {
		return
	}
	a.backgroundMu.Lock()
	defer a.backgroundMu.Unlock()
	if a.backgroundCancel != nil {
		return
	}
	bgCtx, cancel := context.WithCancel(ctx)
	a.backgroundCancel = cancel
	go a.credWarden.Run(bgCtx)
}

// StopBackground stops the daemon-lifetime work. It is called when a gateway is
// replaced by a configuration reload, so the outgoing instance does not leave a
// second warden running against the same credential — two of them would race the
// server's rotation of the refresh token, which is the failure the warden exists
// to prevent. Safe to call more than once, and on a gateway that never started.
func (a *Gateway) StopBackground() {
	if a == nil {
		return
	}
	a.backgroundMu.Lock()
	cancel := a.backgroundCancel
	a.backgroundCancel = nil
	a.backgroundMu.Unlock()
	if cancel != nil {
		cancel()
	}
}
