package gateway

import (
	"context"
	"fmt"

	"github.com/miere/murtaugh/internal/llm"
	"github.com/miere/murtaugh/internal/slack/alertcard"
	slackclient "github.com/miere/murtaugh/internal/slack/client"
)

// alertPoster delivers one alert to the turn's thread as its own message. It is
// a closure rather than an interface because the renderer only ever needs the
// one verb, and the channel and thread are already bound when it is built.
//
// A nil poster means no Slack client is wired (the tests that run the renderer
// headless): the caller falls back to painting alertcard.PlainText on the reply
// surface, which is also what happens when a post fails.
type alertPoster func(ctx context.Context, spec alertcard.Spec) error

// alertMessagePoster is the single Slack verb an alert needs. It is the raw-blocks
// passthrough (slackclient.PostMessageParams.Blocks) rather than the slack-go
// typed builders the rest of this package posts through, because the card is a
// container block: slack-go would silently drop the fields it does not know —
// see "Block Kit rendering" in ARCHITECTURE.md.
type alertMessagePoster interface {
	PostMessage(ctx context.Context, p slackclient.PostMessageParams) (slackclient.PostMessageResult, error)
}

// newAlertPoster binds a renderer and a Slack client to one channel/thread.
//
// The card is posted as its own message rather than appended to the reply for a
// structural reason: a container block cannot be text inside an in-flight
// stream. Sealing the reply first (the renderer's job) and posting below it is
// the same ordering the approval card already uses — see askPermission.
func newAlertPoster(api alertMessagePoster, cards *alertcard.Renderer, channelID, threadTS string) alertPoster {
	if api == nil || cards == nil {
		return nil
	}
	return func(ctx context.Context, spec alertcard.Spec) error {
		blocks, err := cards.Render(spec)
		if err != nil {
			return fmt.Errorf("render alert card: %w", err)
		}
		_, err = api.PostMessage(ctx, slackclient.PostMessageParams{
			ChannelID: channelID,
			ThreadTS:  threadTS,
			// The text field is the notification and screen-reader form, and the
			// fallback Slack shows anywhere blocks do not render.
			Text:   alertcard.FallbackText(spec),
			Blocks: blocks,
		})
		if err != nil {
			return fmt.Errorf("post alert card: %w", err)
		}
		return nil
	}
}

// failSpec turns a turn-ending error into an alert.
//
// A provider failure (any agent backed by internal/llm) is classified, so
// "Gemini is overloaded (503)" reaches the user with the remedy that follows
// from it, instead of a Go error chain wrapped around a JSON body. Everything
// else — ACP transport faults, spawn failures, our own bugs — keeps the generic
// headline.
//
// Either way the unabridged error goes in Detail. That is affordable now in a
// way it was not when this was inline text: the card arrives collapsed, so the
// full diagnostic costs nothing until somebody opens it.
func failSpec(err error) alertcard.Spec {
	spec := alertcard.Spec{Level: alertcard.LevelError}
	if err != nil {
		spec.Detail = err.Error()
	}

	if failure, ok := llm.Classify(err); ok {
		spec.Subtitle = "The agent is not available."
		spec.Reason = failure.String()
		spec.Text = sanitizeSlackInline(failure.Message)
		spec.NextSteps = failure.Remedy()
		return spec
	}

	spec.Subtitle = "Murtaugh hit an error while talking to the agent."
	// Said here rather than left to the card's own fallback, which only applies
	// to a spec with no body at all — and this one has the error in Detail.
	// An unclassified fault is a transport or spawn problem: worth one retry,
	// and somebody's problem if it repeats.
	spec.NextSteps = "Try again. If it keeps happening, notify your admin user."
	return spec
}

// emptyReplySpec is the alert shown when a turn genuinely produced no reply: it
// states what the turn did and stops there.
//
// It deliberately neither hedges nor prescribes. It does not hedge because the
// turn's tool activity is counted rather than guessed at — the old "it may have
// only run tools" was the handler admitting it had not looked. It does not
// prescribe because there is no remedy that is right in every case: an
// interrupted turn never reaches here (it renders the interrupt marker instead),
// and an agent that answered through its Slack tool has already delivered its
// reply, so "nudge it to continue" would be wrong advice in both. That is why
// this carries no NextSteps and the card's own default does not apply — the
// subtitle is the whole message.
//
// The stop reason is left to the log line beside it: `tool_use` means nothing to
// a reader, and it does not identify the failure anyway — the same cancellation
// reports `tool_use` or `end_turn` purely by where in the tool loop it landed.
//
// It is a warning, not an error: nothing broke, but the silence needs
// explaining.
func emptyReplySpec(toolsRun int) alertcard.Spec {
	spec := alertcard.Spec{Level: alertcard.LevelWarn, Title: "No reply"}
	switch {
	case toolsRun == 1:
		spec.Subtitle = "The agent ran one tool and finished without a reply."
	case toolsRun > 1:
		spec.Subtitle = fmt.Sprintf("The agent ran %d tools and finished without a reply.", toolsRun)
	default:
		spec.Subtitle = "The agent finished without a reply."
	}
	// Said explicitly so the card is not given the level's generic guidance,
	// which would be the prescribing this message exists to avoid.
	spec.Text = "_Nothing was lost — the turn simply ended here._"
	return spec
}
