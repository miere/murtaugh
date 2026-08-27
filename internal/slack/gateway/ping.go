package gateway

import (
	"context"

	"github.com/miere/murtaugh/internal/slack/pingcard"
	"github.com/slack-go/slack"
)

// isPingInteraction reports whether the callback is a click on the built-in
// "Test communication" button. handleInteractive dispatches it here — in the
// binary — before the workflow engine, so the ping → pong round-trip can never
// be redirected or broken by a configured workflow rule or an on-disk template.
func isPingInteraction(interaction slack.InteractionCallback) bool {
	if interaction.Type != slack.InteractionTypeBlockActions {
		return false
	}
	for _, action := range interaction.ActionCallback.BlockActions {
		if action == nil {
			continue
		}
		if action.ActionID == pingcard.ActionPing || action.BlockID == pingcard.BlockID {
			return true
		}
	}
	return false
}

// handlePingInteraction posts the pong reply. The copy is a Go constant and the
// reply is sent directly over the Slack messaging surface — no template, no
// response_url, no workflow engine — so the self-test remains functional
// regardless of configuration state. It renders as an info alert card, the same
// shape as every other message Murtaugh sends about itself.
//
// Where it lands depends on where the button was clicked. The button now lives
// in the App Home control row, and a Home-surface click carries no channel at
// all, so the reply goes to the clicker's own DM — the only conversation that
// click implies. A click on a message-hosted card (a startup or back-online
// card posted by an older process, still live in a DM) keeps the old behaviour:
// the pong is threaded under the conversation root so it reads as a reply.
func (a *Gateway) handlePingInteraction(ctx context.Context, interaction slack.InteractionCallback) {
	if a.messaging == nil {
		a.logger.Debug("ping interaction skipped: no Slack messaging wired")
		return
	}
	channel, threadTS := a.pingReplyTarget(ctx, interaction)
	if channel == "" {
		a.logger.Warn("ping interaction has nowhere to reply", "user", interaction.User.ID)
		return
	}
	if _, _, err := a.postLifecycleAlert(ctx, channel, threadTS, pongAlert()); err != nil {
		a.logger.Error("post pong reply failed", "channel", channel, "ts", threadTS, "error", err)
		return
	}
	a.logger.Info("posted pong reply", "channel", channel, "ts", threadTS, "user", interaction.User.ID)
}

// pingReplyTarget resolves the (channel, thread) the pong belongs in. An empty
// channel on the callback means the click came from the App Home, which is not
// a conversation: the clicker's DM is opened instead, and the reply is
// top-level because there is no message to thread it under.
func (a *Gateway) pingReplyTarget(ctx context.Context, interaction slack.InteractionCallback) (string, string) {
	if channel := interaction.Channel.ID; channel != "" {
		threadTS := interaction.Message.ThreadTimestamp
		if threadTS == "" {
			threadTS = interaction.Message.Timestamp
		}
		return channel, threadTS
	}
	user := interaction.User.ID
	if user == "" {
		return "", ""
	}
	convo, _, _, err := a.messaging.OpenConversationContext(ctx, &slack.OpenConversationParameters{Users: []string{user}, ReturnIM: true})
	if err != nil || convo == nil {
		a.logger.Warn("open DM for pong reply failed", "user", user, "error", err)
		return "", ""
	}
	return convo.ID, ""
}
