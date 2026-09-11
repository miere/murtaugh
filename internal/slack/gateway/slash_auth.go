package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/slack/alertcard"
)

const nodeRenewTimeout = 2500 * time.Millisecond

// isAuthSlashCommand reports whether the slash text names the `auth` verb.
func isAuthSlashCommand(text string) bool {
	fields := strings.Fields(text)
	return len(fields) > 0 && strings.EqualFold(fields[0], "auth")
}

func (a *Gateway) handleAuthSlashCommand(event socketmode.Event, command slack.SlashCommand) {
	if a.renewCredential != nil && !authSlashWantsStatus(command.Text) {
		a.ack(event, a.renewNodeCredential(command, slashCommandThreadTS(event)))
		return
	}
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
	// Restart, not Request: the admin typing this verb has usually done so
	// because the last attempt visibly went nowhere, and deferring to it would
	// leave them re-issuing a command that cannot do anything until the lease
	// expires.
	status, replaced := a.credRepair.Restart("manual")
	a.logger.Info("claude_code re-authentication requested via slash command",
		"user", command.UserID, "status", status, "replaced_after", replaced)

	switch status {
	case repairStarted:
		if replaced > 0 {
			a.ack(event, ephemeralText(fmt.Sprintf(
				"Starting Claude Code sign-in — the card is on its way to your DMs. "+
					"(Cancelled the attempt from %s ago.)", replaced.Round(time.Second))))
			return
		}
		a.ack(event, ephemeralText("Starting Claude Code sign-in — the card is on its way to your DMs."))

	case repairAlreadyRunning:
		a.ack(event, ephemeralAlert(alertcard.LevelInfo,
			"A Claude Code sign-in is already running and could not be replaced. Use the card already in your DMs."))

	default:
		a.ack(event, ephemeralAlert(alertcard.LevelError,
			"Could not start the Claude Code sign-in. Check the gateway logs."))
	}
}

func (a *Gateway) renewNodeCredential(command slack.SlashCommand, threadTS string) AckResponse {
	ctx, cancel := context.WithTimeout(context.Background(), nodeRenewTimeout)
	defer cancel()
	admin := a.access().IsAdminUser(command.UserID)
	named := authSlashNode(command.Text)
	conversation := agent.ConversationKey{TeamID: command.TeamID, ChannelID: command.ChannelID, ThreadTS: threadTS, DM: strings.HasPrefix(command.ChannelID, "D")}
	node, err := a.pinnedNode(ctx, conversation)
	switch {
	case err == nil && named != "" && named != node.NodeID:
		return ephemeralAlert(alertcard.LevelWarn, fmt.Sprintf(
			"This conversation runs on node `%s`, not `%s`. Run this in a conversation on `%s`, or somewhere no node is pinned.", node.NodeID, named, named))
	case errors.Is(err, agentruntime.ErrNotPinned) && named == "":
		return ephemeralAlert(alertcard.LevelInfo, "No runtime node is pinned to this conversation, and Murtaugh will not pick one for you. "+
			"Name one with `/murtaugh auth login <node>`. "+a.nameableNodes(command.UserID, admin))
	case errors.Is(err, agentruntime.ErrNotPinned):
		if node, err = a.connectedNode(named, command.UserID, admin); err != nil {
			return ephemeralAlert(alertcard.LevelWarn, err.Error()+" "+a.nameableNodes(command.UserID, admin))
		}
	case err != nil:
		return ephemeralAlert(alertcard.LevelError, "Could not find the node this conversation runs on: "+err.Error())
	}
	if !admin && command.UserID != node.Owner {
		a.logger.Info("denied auth slash command from someone who neither administers the gateway nor owns the node",
			"user", command.UserID, "node_id", node.NodeID, "channel", command.ChannelID)
		return ephemeralAlert(alertcard.LevelWarn, "Only the admin, or the owner of the node this conversation runs on, can sign it in again.")
	}
	if !a.access().IsAllowedUser(node.Owner) {
		return ephemeralAlert(alertcard.LevelWarn, fmt.Sprintf(
			"The owner of node `%s` may no longer use this gateway, so nobody can be asked to sign it in.", node.NodeID))
	}
	status, err := a.renewCredential(ctx, node.NodeID)
	a.logger.Info("claude_code re-authentication requested on a node via slash command",
		"user", command.UserID, "node_id", node.NodeID, "status", status, "error", err)
	switch {
	case err != nil:
		return ephemeralAlert(alertcard.LevelError, fmt.Sprintf("Could not ask node `%s` to sign in again: %v", node.NodeID, err))
	case status == agentruntime.RenewalStarted:
		return ephemeralText(fmt.Sprintf("Asked node `%s` to sign Claude Code in again — the card is on its way to <@%s>'s DMs.", node.NodeID, node.Owner))
	case status == agentruntime.RenewalAlreadyRunning:
		return ephemeralAlert(alertcard.LevelWarn, fmt.Sprintf(
			"Node `%s` has an earlier Claude Code sign-in that would not stop, so a new one was not started. Its card is in <@%s>'s DMs.", node.NodeID, node.Owner))
	}
	return ephemeralAlert(alertcard.LevelInfo, fmt.Sprintf("Node `%s` runs no claude_code agent, so there is nothing to sign in.", node.NodeID))
}

func (a *Gateway) connectedNode(nodeID, userID string, admin bool) (agentruntime.NodeRef, error) {
	for _, node := range a.connectedNodes() {
		if node.NodeID != nodeID {
			continue
		}
		if !admin && node.Owner != userID {
			return agentruntime.NodeRef{}, fmt.Errorf("Node `%s` is not yours, so you cannot sign it in.", nodeID)
		}
		return node, nil
	}
	return agentruntime.NodeRef{}, fmt.Errorf("No node called `%s` is connected.", nodeID)
}

func (a *Gateway) nameableNodes(userID string, admin bool) string {
	var names []string
	for _, node := range a.connectedNodes() {
		if admin || node.Owner == userID {
			names = append(names, "`"+node.NodeID+"`")
		}
	}
	if len(names) == 0 {
		return "No connected node is yours to name."
	}
	return "Nodes you may name: " + strings.Join(names, ", ") + "."
}

func authSlashNode(text string) string {
	fields := strings.Fields(text)
	if len(fields) > 2 && strings.EqualFold(fields[1], "login") {
		return fields[2]
	}
	return ""
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
	if a.credReports != nil {
		return nodeCredentialStatusText(a.credReports(), time.Now())
	}
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

func nodeCredentialStatusText(reports []agentruntime.CredentialHealth, now time.Time) string {
	if len(reports) == 0 {
		return "No connected runtime node has reported a Claude Code credential. " +
			"A node reports each one it watches when it connects, and again whenever one starts or stops failing."
	}
	var b strings.Builder
	b.WriteString("*Claude Code credentials on runtime nodes*\n")
	for _, r := range reports {
		fmt.Fprintf(&b, "• node `%s` (<@%s>): `%s`\n", r.NodeID, r.Owner, r.Credential)
		if r.Degraded {
			fmt.Fprintf(&b, "    *failing* since %s ago\n", now.Sub(r.Since).Round(time.Minute).String())
		} else {
			b.WriteString("    working\n")
		}
		if r.ExpiresAt.IsZero() {
			b.WriteString("    expiry: _not yet read_\n")
		} else {
			fmt.Fprintf(&b, "    expiry: %s\n", relativeExpiry(r.ExpiresAt, now))
		}
		if r.Reason != "" {
			fmt.Fprintf(&b, "    :warning: %s\n", r.Reason)
		}
		fmt.Fprintf(&b, "    reported %s ago\n", now.Sub(r.ReportedAt).Round(time.Second).String())
	}
	return strings.TrimRight(b.String(), "\n")
}
