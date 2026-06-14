// Package restartcard is the single source of truth for the restart-approval
// Block Kit card: its layout plus the stable block_id / action_id values the
// gateway's interaction router keys on.
//
// It lives in its own package — rather than in internal/slack/gateway — so a
// card posted from a process that has no gateway (the `restart` tool running
// inside `murtaugh mcp` or the CLI) is byte-for-byte the card the running
// gateway daemon recognizes when its Confirm button is clicked. Keeping the
// IDs here avoids a tools → gateway import cycle.
package restartcard

import (
	"strings"

	"github.com/slack-go/slack"
)

const (
	// BlockID tags the actions block that carries the confirm/dismiss buttons.
	// The router uses it (and the action_id prefix below) to identify the
	// callback without depending on free-form text.
	BlockID = "murtaugh_restart_suggestion"

	// ActionPrefix namespaces every action_id the card emits.
	ActionPrefix = "murtaugh_restart_suggestion_"
	// ActionConfirm fires the restart (admin-gated by the handler).
	ActionConfirm = "murtaugh_restart_suggestion_confirm"
	// ActionDismiss discards the suggestion.
	ActionDismiss = "murtaugh_restart_suggestion_dismiss"

	// Headline is the card's section text and the message's notification fallback.
	Headline = ":warning: Murtaugh thinks a restart might help."

	// DefaultReason is rendered when the caller supplies no reason.
	DefaultReason = "Murtaugh detected a condition that may be resolved by a restart."
)

// Build returns the Block Kit layout for the restart-approval card. The reason
// is rendered as a context line so the operator knows why the restart is being
// requested; the two buttons carry the stable action_ids consumed by the
// gateway's interactive handler, and the button value carries the reason back
// for audit when the operator clicks.
func Build(reason string) []slack.Block {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = DefaultReason
	}
	confirm := slack.NewButtonBlockElement(
		ActionConfirm,
		reason,
		slack.NewTextBlockObject(slack.PlainTextType, "Restart now", false, false),
	)
	confirm.Style = slack.StylePrimary
	dismiss := slack.NewButtonBlockElement(
		ActionDismiss,
		reason,
		slack.NewTextBlockObject(slack.PlainTextType, "Dismiss", false, false),
	)
	return []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, Headline, false, false), nil, nil),
		slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, reason, false, false)),
		slack.NewActionBlock(BlockID, confirm, dismiss),
	}
}
