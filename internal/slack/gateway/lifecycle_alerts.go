package gateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/miere/murtaugh/internal/slack/alertcard"
	"github.com/miere/murtaugh/internal/slack/pingcard"
	"github.com/slack-go/slack"
)

// errNoSlackMessaging reports that neither delivery path is wired. Callers of
// the lifecycle helpers are all best-effort, so they log it and carry on.
var errNoSlackMessaging = errors.New("no Slack messaging surface is wired")

// The messages Murtaugh sends about its own lifecycle — it started, it is
// restarting, it is back, the link is healthy. None of them is a failure, so
// every one is an alertcard.LevelInfo: the same collapsed container, icon and
// vocabulary the rest of Murtaugh's self-reporting uses. They were four
// bespoke one-line notices before, each with its own emoji.
//
// They are specs rather than rendered blocks because two of them are the same
// message at two points in time: the restart notice is posted by the process on
// its way out and edited in place into the back-online card by the process that
// comes back (see resume.go). Keeping them as data lets that edit re-render the
// same card rather than swap layouts.

// startupAlert is the greeting posted to the admin DM on a normal boot.
//
// It carries next steps because the Test-communication button no longer rides
// on this message: the self-test, Restart and Upgrade all live in the App Home
// control row now, and this is where an operator finds that out.
func startupAlert() alertcard.Spec {
	return alertcard.Spec{
		Level:     alertcard.LevelNotice,
		Title:     "Murtaugh has started",
		Subtitle:  "The server is up and connected to Slack.",
		NextSteps: "Open Murtaugh's App Home to restart, update, or test communication.",
	}
}

// restartNoticeAlert is posted to the originating conversation immediately
// before the daemon exits. consumeResumeMarker edits this exact message into
// backOnlineAlert when the replacement process connects, so the restart costs
// the operator one message rather than three.
func restartNoticeAlert() alertcard.Spec {
	return alertcard.Spec{
		Level:     alertcard.LevelNotice,
		Title:     "Restarting Murtaugh…",
		Subtitle:  "The daemon is going down; its supervisor brings it straight back.",
		NextSteps: "Nothing to do — this message updates itself once Murtaugh is back.",
	}
}

// backOnlineAlert replaces restartNoticeAlert in place once the new process has
// connected to Slack.
func backOnlineAlert() alertcard.Spec {
	return alertcard.Spec{
		Level:     alertcard.LevelNotice,
		Title:     "Murtaugh is back online",
		Subtitle:  "The restart finished and the Slack connection is live.",
		NextSteps: "Any conversation that was in flight when the restart began was interrupted; ask again to resume it.",
	}
}

// configReloadingAlert is posted once an admin approves a configuration change.
//
// It mirrors restartNoticeAlert on purpose: from the admin's side a soft reload
// and a restart are the same experience — the bot goes quiet, agents drop their
// work, and it comes back — so it should read the same rather than making them
// learn a second vocabulary for it.
func configReloadingAlert() alertcard.Spec {
	return alertcard.Spec{
		Level:     alertcard.LevelNotice,
		Title:     "Reloading the configuration…",
		Subtitle:  "The approved changes are being applied; agents are restarting.",
		NextSteps: "Any conversation that was in flight has been stopped; ask again once Murtaugh is back.",
	}
}

// configReloadedAlert confirms the new configuration is live.
func configReloadedAlert() alertcard.Spec {
	return alertcard.Spec{
		Level:    alertcard.LevelNotice,
		Title:    "Configuration reloaded",
		Subtitle: "Murtaugh is running the approved configuration.",
	}
}

// updateRestartingAlert announces a self-update that installed cleanly and is
// taking the daemon down to run itself.
//
// It is a NOTICE for the same reason restartNoticeAlert is: the update went
// fine, the restart is automatic, and there is no decision for the operator to
// make. It was a bespoke one-line ":arrows_counterclockwise:" DM before, which
// is exactly the shape this level exists to replace — the lifecycle messages
// speak one vocabulary or they speak none.
func updateRestartingAlert(version string) alertcard.Spec {
	return alertcard.Spec{
		Level:    alertcard.LevelNotice,
		Title:    fmt.Sprintf("Updated to %s", version),
		Subtitle: "Restarting now to run the new build.",
	}
}

// updateInstalledAlert announces an update that landed on disk with no restart
// coordinator to run it.
//
// This one is INFO rather than a notice: the new build is installed and the old
// one is still serving, so the operator has to act before the update means
// anything. A notice is a passing remark that needs no decision — this needs
// one, and the card is where NextSteps can say so.
func updateInstalledAlert(version string) alertcard.Spec {
	return alertcard.Spec{
		Level:     alertcard.LevelInfo,
		Title:     fmt.Sprintf("Updated to %s", version),
		Subtitle:  "The new build is installed but not running yet.",
		NextSteps: "Restart Murtaugh to run it.",
	}
}

// pongAlert is the reply to a click on the App Home's Test-communication
// button: the round-trip completed, which is the whole content of the message.
func pongAlert() alertcard.Spec {
	return alertcard.Spec{
		Level:    alertcard.LevelInfo,
		Title:    pingcard.PongTitle,
		Subtitle: pingcard.PongSubtitle,
	}
}

// postNotice posts the discreet one-line form: a single context block carrying
// plain text, exactly as statusMsgOptions builds for the idle-timeout nudge.
//
// It goes through the ordinary messaging client rather than the raw-blocks
// passthrough, because a context block is not one of the newer types slack-go
// cannot express — the passthrough exists for the container card, and a notice
// is not one.
func (a *Gateway) postNotice(ctx context.Context, channel, threadTS, text string) (string, string, error) {
	if a.messaging == nil {
		return "", "", errNoSlackMessaging
	}
	options := statusMsgOptions(text)
	if threadTS != "" {
		options = append(options, slack.MsgOptionTS(threadTS))
	}
	return a.messaging.PostMessageContext(ctx, channel, options...)
}

// updateNotice rewrites a posted notice in place, keeping its shape.
func (a *Gateway) updateNotice(ctx context.Context, channel, ts, text string) error {
	if a.messaging == nil {
		return errNoSlackMessaging
	}
	if _, _, _, err := a.messaging.UpdateMessageContext(ctx, channel, ts, statusMsgOptions(text)...); err != nil {
		return fmt.Errorf("update notice: %w", err)
	}
	return nil
}

// postLifecycleAlert posts spec as an info card and reports where it landed.
//
// The card needs the raw-blocks passthrough (a container block; see alert.go),
// and that client is not always wired — a token that failed to build one, or a
// struct-literal gateway in a test. Rather than go silent on the path that
// announces a restart, it degrades to alertcard.PlainText over the same
// messaging surface the notice used before it was a card. The returned channel
// and TS are identical either way, which is what lets the resume marker point
// at the message regardless of which path posted it.
func (a *Gateway) postLifecycleAlert(ctx context.Context, channel, threadTS string, spec alertcard.Spec) (string, string, error) {
	// A notice is not a card. It is the same discreet context line the
	// idle-timeout nudge uses — one grey sentence under the thing it refers to,
	// built with the shared helper so the two shapes cannot drift.
	if spec.Level == alertcard.LevelNotice {
		return a.postNotice(ctx, channel, threadTS, alertcard.NoticeText(spec))
	}
	if a.alertAPI != nil && a.alertCards != nil {
		res, err := postAlertCard(ctx, a.alertAPI, a.alertCards, channel, threadTS, spec)
		if err == nil {
			return res.Channel, res.TS, nil
		}
		a.logger.Warn("failed to post lifecycle alert card; falling back to text", "channel", channel, "error", err)
	}
	if a.messaging == nil {
		return "", "", errNoSlackMessaging
	}
	options := []slack.MsgOption{slack.MsgOptionText(alertcard.PlainText(spec), false)}
	if threadTS != "" {
		options = append(options, slack.MsgOptionTS(threadTS))
	}
	return a.messaging.PostMessageContext(ctx, channel, options...)
}

// updateLifecycleAlert re-renders an already-posted lifecycle message as spec,
// with the same card-then-text degradation as postLifecycleAlert.
//
// The text fallback clears the blocks explicitly: the message being edited may
// have been posted as a card by the previous process even though this one has
// no raw-blocks client, and an edit that only replaces the text would leave the
// stale card visible underneath.
func (a *Gateway) updateLifecycleAlert(ctx context.Context, channel, ts string, spec alertcard.Spec) error {
	if spec.Level == alertcard.LevelNotice {
		return a.updateNotice(ctx, channel, ts, alertcard.NoticeText(spec))
	}
	if a.alertEditor != nil && a.alertCards != nil {
		err := updateAlertCard(ctx, a.alertEditor, a.alertCards, channel, ts, spec)
		if err == nil {
			return nil
		}
		a.logger.Warn("failed to update lifecycle alert card; falling back to text", "channel", channel, "ts", ts, "error", err)
	}
	if a.messaging == nil {
		return errNoSlackMessaging
	}
	_, _, _, err := a.messaging.UpdateMessageContext(ctx, channel, ts,
		slack.MsgOptionText(alertcard.PlainText(spec), false),
		slack.MsgOptionBlocks(),
	)
	return err
}
