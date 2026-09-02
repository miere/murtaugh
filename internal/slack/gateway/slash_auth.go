package gateway

import (
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/miere/murtaugh/internal/slack/alertcard"
)

// isAuthSlashCommand reports whether the slash text names the `auth` verb.
func isAuthSlashCommand(text string) bool {
	fields := strings.Fields(text)
	return len(fields) > 0 && strings.EqualFold(fields[0], "auth")
}

// handleAuthSlashCommand implements `<command> auth [status]`.
//
// It is the operator's way in. The credential warden is deliberately internal —
// not a config-store job — so that no agent can enumerate, run, redefine or
// silently disable it. The cost of that is losing the visibility a job would
// have given, and this verb pays it back: `auth status` reports what the warden
// sees, and the bare verb starts a re-authentication before a lockout rather
// than after one.
//
// Admin-only, on the same two-layer model as `restart`: handleSlashCommand has
// already checked IsAllowedUser, and credentials belong to the admin alone. A
// non-admin gets an explicit deny rather than silence, so the boundary is
// discoverable.
func (a *Gateway) handleAuthSlashCommand(event socketmode.Event, command slack.SlashCommand) {
	if !a.access().IsAdminUser(command.UserID) {
		a.logger.Info("denied auth slash command from non-admin user",
			"command", command.Command, "user", command.UserID, "channel", command.ChannelID)
		a.ack(event, ephemeralAlert(alertcard.LevelWarn, "Only the configured admin can manage Murtaugh's credentials."))
		return
	}

	if authSlashWantsStatus(command.Text) {
		a.ack(event, ephemeralText(a.credentialStatusText()))
		return
	}

	if a.credRepair == nil {
		a.ack(event, ephemeralAlert(alertcard.LevelInfo,
			"Re-authentication is not available in this deployment (no auth flow is wired)."))
		return
	}
	if !a.credRepair.Request("manual") {
		a.ack(event, ephemeralAlert(alertcard.LevelError,
			"Could not start the Claude Code sign-in. Check the gateway logs."))
		return
	}
	a.logger.Info("claude_code re-authentication requested via slash command", "user", command.UserID)
	a.ack(event, ephemeralText("Starting Claude Code sign-in — the card is on its way to your DMs."))
}

// authSlashWantsStatus reports whether the verb was `auth status` rather than a
// bare `auth`. Anything other than an explicit `status` is treated as the
// re-authentication request, so a typo cannot silently do nothing.
func authSlashWantsStatus(text string) bool {
	fields := strings.Fields(text)
	return len(fields) > 1 && strings.EqualFold(fields[1], "status")
}

// credentialStatusText renders the warden's view of every watched credential.
//
// It carries no secret material — only timings and the last error — which is
// what makes it safe to render into an ephemeral Slack message and, by the same
// token, into the diagnostics bundle.
func (a *Gateway) credentialStatusText() string {
	if a.credWarden == nil {
		return "No `claude_code` agent is configured, so no Claude Code credential is being watched."
	}
	states := a.credWarden.States()
	if len(states) == 0 {
		return "The credential warden is running but has not observed any credential yet."
	}

	var b strings.Builder
	b.WriteString("*Claude Code credentials*\n")
	now := time.Now()
	for _, s := range states {
		fmt.Fprintf(&b, "• `%s`\n", s.Identity.String())
		switch {
		case s.ExpiresAt.IsZero():
			b.WriteString("    expiry: _not yet read_\n")
		default:
			remaining := s.ExpiresAt.Sub(now).Round(time.Minute)
			if remaining < 0 {
				fmt.Fprintf(&b, "    expiry: *lapsed* %s ago\n", (-remaining).String())
			} else {
				fmt.Fprintf(&b, "    expiry: in %s\n", remaining.String())
			}
		}
		if !s.LastRefresh.IsZero() {
			fmt.Fprintf(&b, "    last refresh: %s ago (%d this run)\n",
				now.Sub(s.LastRefresh).Round(time.Minute).String(), s.Refreshes)
		} else {
			b.WriteString("    last refresh: _none this run_\n")
		}
		if !s.NextCheck.IsZero() {
			if d := s.NextCheck.Sub(now).Round(time.Minute); d > 0 {
				fmt.Fprintf(&b, "    next check: in %s\n", d.String())
			} else {
				b.WriteString("    next check: _due now_\n")
			}
		}
		if s.Attempts > 0 {
			fmt.Fprintf(&b, "    attempts against the current expiry: %d\n", s.Attempts)
		}
		if s.LastError != "" {
			fmt.Fprintf(&b, "    :warning: %s\n", s.LastError)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
