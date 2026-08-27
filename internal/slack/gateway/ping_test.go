package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/slack/pingcard"
	"github.com/miere/murtaugh/internal/slack/restartcard"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// pingClick synthesises the block_actions callback Slack delivers when the
// "Test communication" button is pressed on a MESSAGE — the startup or
// back-online card posted by an older process, still live in a DM. threadTS is
// the clicked message's own thread_ts (empty for a top-level card; set for one
// that is itself a threaded reply). See appHomePingClick for the current
// surface.
func pingClick(user, channel, messageTS, threadTS string) slack.InteractionCallback {
	return slack.InteractionCallback{
		Type:    slack.InteractionTypeBlockActions,
		User:    slack.User{ID: user},
		Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: channel}}},
		Message: slack.Message{Msg: slack.Msg{Timestamp: messageTS, ThreadTimestamp: threadTS}},
		ActionCallback: slack.ActionCallbacks{BlockActions: []*slack.BlockAction{{
			BlockID:  pingcard.BlockID,
			ActionID: pingcard.ActionPing,
		}}},
	}
}

func TestIsPingInteraction(t *testing.T) {
	if !isPingInteraction(pingClick("U1", "C1", "1.0", "")) {
		t.Fatal("expected the Test-communication click to be recognised")
	}
	// action_id alone (no block_id) must still be recognised.
	byAction := pingClick("U1", "C1", "1.0", "")
	byAction.ActionCallback.BlockActions[0].BlockID = ""
	if !isPingInteraction(byAction) {
		t.Fatal("expected recognition by action_id alone")
	}
	foreign := suggestionInteraction("U1", "C1", "1.0", restartcard.ActionConfirm, "x")
	if isPingInteraction(foreign) {
		t.Fatal("did not expect a restart-suggestion click to be treated as a ping")
	}
	if isPingInteraction(slack.InteractionCallback{Type: slack.InteractionTypeShortcut}) {
		t.Fatal("non block_actions callback should never be recognised")
	}
}

func TestHandlePingInteractionThreadsUnderTopLevelCard(t *testing.T) {
	msg := &recordingMessaging{}
	app := &Gateway{logger: newSilentLogger(), messaging: msg}
	// A top-level startup card has no thread_ts, so the pong threads under the
	// card's own ts.
	app.handlePingInteraction(context.Background(), pingClick("U1", "C1", "1700000000.000100", ""))
	if msg.postCalls != 1 || msg.postChannel != "C1" {
		t.Fatalf("expected one pong post to C1, got calls=%d channel=%q", msg.postCalls, msg.postChannel)
	}
	if msg.postThreadTS != "1700000000.000100" {
		t.Fatalf("expected pong threaded under the card ts, got thread_ts=%q", msg.postThreadTS)
	}
}

func TestHandlePingInteractionThreadsUnderExistingThread(t *testing.T) {
	msg := &recordingMessaging{}
	app := &Gateway{logger: newSilentLogger(), messaging: msg}
	// The back-online card is itself a reply (thread_ts set); the pong must join
	// that same thread root, not nest under the reply.
	app.handlePingInteraction(context.Background(), pingClick("U1", "C1", "1700000000.000200", "1700000000.000100"))
	if msg.postThreadTS != "1700000000.000100" {
		t.Fatalf("expected pong threaded under the conversation root, got thread_ts=%q", msg.postThreadTS)
	}
}

// appHomePingClick synthesises the callback for a click in the App Home control
// row — the button's home now. Slack sends no channel and no message for a
// Home-surface click, which is exactly what the handler has to cope with.
func appHomePingClick(user string) slack.InteractionCallback {
	return slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
		User: slack.User{ID: user},
		ActionCallback: slack.ActionCallbacks{BlockActions: []*slack.BlockAction{{
			BlockID:  appHomeActionsBlockID,
			ActionID: pingcard.ActionPing,
		}}},
	}
}

func TestIsPingInteractionRecognisesTheAppHomeButton(t *testing.T) {
	if !isPingInteraction(appHomePingClick("U1")) {
		t.Fatal("expected the App Home Test-communication click to be recognised")
	}
}

// A Home-surface click carries no conversation, so the pong goes to the
// clicker's own DM rather than being dropped for want of a channel.
func TestHandlePingInteractionRepliesInTheClickersDMFromAppHome(t *testing.T) {
	msg := &recordingMessaging{openChannelID: "DADMIN"}
	app := &Gateway{logger: newSilentLogger(), messaging: msg}
	app.handlePingInteraction(context.Background(), appHomePingClick("U1"))
	if len(msg.openUsers) != 1 || msg.openUsers[0] != "U1" {
		t.Fatalf("expected a DM opened with the clicker, got %#v", msg.openUsers)
	}
	if msg.postCalls != 1 || msg.postChannel != "DADMIN" {
		t.Fatalf("expected one pong post to DADMIN, got calls=%d channel=%q", msg.postCalls, msg.postChannel)
	}
	// Nothing to thread under: the click came from a surface with no messages.
	if msg.postThreadTS != "" {
		t.Fatalf("expected a top-level pong, got thread_ts=%q", msg.postThreadTS)
	}
}

// The pong is an info card like every other lifecycle message, so when a
// raw-blocks client is wired it must go through it.
func TestHandlePingInteractionPostsAnInfoCard(t *testing.T) {
	poster := &fakeAlertAPI{}
	msg := &recordingMessaging{}
	app := &Gateway{
		logger:     newSilentLogger(),
		messaging:  msg,
		alertCards: testAlertCards(),
		alertAPI:   poster,
	}
	app.handlePingInteraction(context.Background(), pingClick("U1", "C1", "1700000000.000100", ""))
	if msg.postCalls != 0 {
		t.Fatalf("expected no text fallback, got %d text post(s)", msg.postCalls)
	}
	if len(poster.posts) != 1 {
		t.Fatalf("expected one pong card, got %d", len(poster.posts))
	}
	got := poster.posts[0]
	if got.ChannelID != "C1" || got.ThreadTS != "1700000000.000100" {
		t.Fatalf("pong card landed on %q/%q, want C1 threaded under the card", got.ChannelID, got.ThreadTS)
	}
	assertInfoAlertCard(t, got.Blocks)
	if !strings.Contains(got.Text, pingcard.PongSubtitle) {
		t.Fatalf("expected the pong's notification text to carry %q, got %q", pingcard.PongSubtitle, got.Text)
	}
}

// TestHandleInteractiveRoutesPingAwayFromWorkflow verifies the gateway handles
// the ping click itself, before the workflow engine — the whole point of moving
// the self-test into the binary.
func TestHandleInteractiveRoutesPingAwayFromWorkflow(t *testing.T) {
	wf := &recordingWorkflow{}
	msg := &recordingMessaging{}
	app := &Gateway{
		workflow:  wf,
		messaging: msg,
		logger:    newSilentLogger(),
		cfg:       config.AccessConfig{AllowedUsers: []string{"U1"}},
	}
	app.handleInteractive(socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: pingClick("U1", "C1", "1700000000.000100", ""),
	})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if msg.recordedPostCalls() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if calls, _ := wf.stats(); calls != 0 {
		t.Fatalf("expected workflow engine to be bypassed for ping clicks, got %d calls", calls)
	}
	if got := msg.recordedPostCalls(); got != 1 {
		t.Fatalf("expected exactly one pong post from the gateway, got %d", got)
	}
}
