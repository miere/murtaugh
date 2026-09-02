package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/slack/alertcard"
	slackclient "github.com/miere/murtaugh/internal/slack/client"
)

// fakeAlertEditor captures the raw-block edits a lifecycle alert makes. It is
// the update half of fakeAlertAPI; the real client is one object satisfying
// both, so tests that need the pair embed this alongside it.
type fakeAlertEditor struct {
	updates   []slackclient.UpdateMessageParams
	updateErr error
}

func (f *fakeAlertEditor) UpdateMessage(_ context.Context, p slackclient.UpdateMessageParams) (slackclient.PostMessageResult, error) {
	if f.updateErr != nil {
		return slackclient.PostMessageResult{}, f.updateErr
	}
	f.updates = append(f.updates, p)
	return slackclient.PostMessageResult{Channel: p.ChannelID, TS: p.TS}, nil
}

// assertInfoAlertCard decodes rendered blocks and asserts they are the shared
// alert container at info severity — the check that keeps a lifecycle message
// from quietly drifting back to a bespoke layout or a louder level.
func assertInfoAlertCard(t *testing.T, blocks []byte) {
	t.Helper()
	if len(blocks) == 0 {
		t.Fatal("expected rendered blocks, got none")
	}
	var payload struct {
		Blocks []struct {
			Type    string `json:"type"`
			BlockID string `json:"block_id"`
			Icon    struct {
				ImageURL string `json:"image_url"`
				AltText  string `json:"alt_text"`
			} `json:"icon"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(blocks, &payload); err != nil {
		t.Fatalf("decode blocks: %v\n%s", err, blocks)
	}
	if len(payload.Blocks) != 1 {
		t.Fatalf("expected one container block, got %d:\n%s", len(payload.Blocks), blocks)
	}
	top := payload.Blocks[0]
	if top.Type != "container" || top.BlockID != alertcard.ContainerBlockID {
		t.Fatalf("expected the shared alert container, got type=%q block_id=%q", top.Type, top.BlockID)
	}
	// The icon is how the level reaches the eye, and it is the only part of the
	// rendered card that distinguishes info from error.
	if top.Icon.AltText != "Information icon" {
		t.Fatalf("expected the info icon, got alt_text %q", top.Icon.AltText)
	}
}

// A restart is one message in two states: the notice posted on the way out is
// the message the returning process edits.
//
// Both are NOTICES — the discreet one-line form the idle-timeout nudge uses —
// so they go through the ordinary messaging client, not the raw-blocks
// passthrough the container card needs.
func TestRestartNoticeAndBackOnlineRenderAsNotices(t *testing.T) {
	poster := &fakeAlertAPI{}
	msg := &recordingMessaging{postReturnedTS: "111.222"}
	app := &Gateway{
		logger:      newSilentLogger(),
		messaging:   msg,
		alertCards:  testAlertCards(),
		alertAPI:    poster,
		alertEditor: &fakeAlertEditor{},
	}

	channel, ts, err := app.postLifecycleAlert(context.Background(), "C1", "", restartNoticeAlert())
	if err != nil {
		t.Fatalf("postLifecycleAlert returned error: %v", err)
	}
	if len(poster.posts) != 0 {
		t.Fatalf("a notice was posted as a card; it must take the plain messaging path")
	}
	if msg.postCalls != 1 {
		t.Fatalf("expected one notice post, got %d", msg.postCalls)
	}

	if err := app.updateLifecycleAlert(context.Background(), channel, ts, backOnlineAlert()); err != nil {
		t.Fatalf("updateLifecycleAlert returned error: %v", err)
	}
	if msg.updateCalls != 1 {
		t.Fatalf("expected one notice edit, got %d", msg.updateCalls)
	}
	// The edit must land on the message the post created; that identity is what
	// makes the confirmation replace the notice instead of stacking below it.
	if msg.updateChannel != channel || msg.updateTS != ts {
		t.Fatalf("edit targeted %q/%q, want the posted message %q/%q",
			msg.updateChannel, msg.updateTS, channel, ts)
	}
}

// With no raw-blocks client the lifecycle messages still have to arrive: the
// restart notice in particular is what the resume marker points at, so going
// silent would strand the marker. They degrade to the card's plain-text form
// over the typed messaging surface.
func TestLifecycleAlertsFallBackToTextWithoutACardClient(t *testing.T) {
	msg := &recordingMessaging{}
	app := &Gateway{logger: newSilentLogger(), messaging: msg}

	if _, _, err := app.postLifecycleAlert(context.Background(), "C1", "", restartNoticeAlert()); err != nil {
		t.Fatalf("postLifecycleAlert returned error: %v", err)
	}
	if msg.postCalls != 1 {
		t.Fatalf("expected one text post, got %d", msg.postCalls)
	}
	if err := app.updateLifecycleAlert(context.Background(), "C1", "1.0", backOnlineAlert()); err != nil {
		t.Fatalf("updateLifecycleAlert returned error: %v", err)
	}
	if msg.updateCalls != 1 {
		t.Fatalf("expected one text edit, got %d", msg.updateCalls)
	}
	// Two options: the text AND an empty blocks option. The message being edited
	// may carry a card posted by a process that DID have a raw-blocks client, and
	// replacing only the text would leave that stale card visible underneath.
	if msg.updateOptions < 2 {
		t.Fatalf("expected the text fallback to clear the blocks too, got %d option(s)", msg.updateOptions)
	}
}

// A card post that fails must not swallow the message — it falls through to the
// text path, which is the difference between a degraded notice and no notice.
func TestPostLifecycleAlertFallsBackWhenTheCardPostFails(t *testing.T) {
	msg := &recordingMessaging{}
	app := &Gateway{
		logger:     newSilentLogger(),
		messaging:  msg,
		alertCards: testAlertCards(),
		alertAPI:   &fakeAlertAPI{postErr: errors.New("invalid_blocks")},
	}
	if _, _, err := app.postLifecycleAlert(context.Background(), "C1", "", restartNoticeAlert()); err != nil {
		t.Fatalf("postLifecycleAlert returned error: %v", err)
	}
	if msg.postCalls != 1 {
		t.Fatalf("expected the failed card post to fall back to text, got %d text post(s)", msg.postCalls)
	}
}

// Every lifecycle message is a statement of fact about Murtaugh itself, never a
// failure — and none of them needs a card. They arrive next to the thing they
// refer to (an approval card, a setup prompt), where a second full-width block
// repeats what is already on screen at the same cost in space.
func TestLifecycleAlertsAreAllNotices(t *testing.T) {
	for name, spec := range map[string]alertcard.Spec{
		"startup":          startupAlert(),
		"restartNotice":    restartNoticeAlert(),
		"backOnline":       backOnlineAlert(),
		"configReloading":  configReloadingAlert(),
		"configReloaded":   configReloadedAlert(),
		"updateRestarting": updateRestartingAlert("v0.34.2"),
	} {
		if spec.Level != alertcard.LevelNotice {
			t.Errorf("%s alert has level %q, want %q", name, spec.Level, alertcard.LevelNotice)
		}
		if strings.TrimSpace(spec.Title) == "" || strings.TrimSpace(spec.Subtitle) == "" {
			t.Errorf("%s alert needs both a title and a subtitle; they are the whole message while the card is collapsed", name)
		}
	}
}

// The update announcements carry the version in the headline, since that is the
// half of a notice a user actually reads, and the restarting one must not be a
// card: it was a bespoke ":arrows_counterclockwise:" DM, which is the shape the
// notice level exists to retire.
func TestUpdateAlertsSpeakTheSharedVocabulary(t *testing.T) {
	restarting := updateRestartingAlert("v0.34.2")
	if !strings.Contains(restarting.Title, "v0.34.2") {
		t.Errorf("restarting notice %q does not name the version", restarting.Title)
	}
	if strings.Contains(alertcard.NoticeText(restarting), ":") {
		t.Errorf("a notice carries no bespoke emoji: %q", alertcard.NoticeText(restarting))
	}

	// The no-coordinator branch is NOT a notice: the new build is installed but
	// the old one is still serving, so the operator has to do something. That
	// needs a card with next steps, which a notice cannot carry.
	installed := updateInstalledAlert("v0.34.2")
	if installed.Level != alertcard.LevelInfo {
		t.Errorf("installed-not-running alert has level %q, want %q", installed.Level, alertcard.LevelInfo)
	}
	if strings.TrimSpace(installed.NextSteps) == "" {
		t.Error("installed-not-running alert must tell the operator to restart")
	}
}

// notifyAdminAlert is the only way the update announcements reach Slack, so it
// has to honour the level rather than always posting a card: a notice through
// the admin DM must be the same one-line context message it is everywhere else.
func TestNotifyAdminAlertRoutesOnTheLevel(t *testing.T) {
	newGW := func(msg *recordingMessaging, poster *fakeAlertAPI) *Gateway {
		return &Gateway{
			logger:     newSilentLogger(),
			cfg:        config.AccessConfig{AdminUser: "UADMIN00"},
			messaging:  msg,
			alertCards: testAlertCards(),
			alertAPI:   poster,
		}
	}

	msg, poster := &recordingMessaging{}, &fakeAlertAPI{}
	newGW(msg, poster).notifyAdminAlert(context.Background(), updateRestartingAlert("v0.34.2"))
	if len(poster.posts) != 0 {
		t.Fatalf("the update notice was posted as a card; it must take the notice path")
	}
	if msg.postCalls != 1 {
		t.Fatalf("expected one notice post to the admin DM, got %d", msg.postCalls)
	}
	if !strings.Contains(msg.postText, "v0.34.2") {
		t.Errorf("notice text %q does not name the installed version", msg.postText)
	}

	// An info alert still gets the card it was always getting.
	msg, poster = &recordingMessaging{}, &fakeAlertAPI{}
	newGW(msg, poster).notifyAdminAlert(context.Background(), updateInstalledAlert("v0.34.2"))
	if len(poster.posts) != 1 {
		t.Fatalf("expected the info alert to post one card, got %d", len(poster.posts))
	}
}

// TestNoticeMatchesTheNudgeShape is the whole point of the level.
//
// A notice must render as the same discreet grey line the idle-timeout nudge
// uses — one context block carrying plain_text with emoji enabled. Asserting it
// against statusMsgOptions rather than against a hand-written expectation is
// what stops the two drifting: if the nudge's shape changes, this changes with
// it or fails.
func TestNoticeMatchesTheNudgeShape(t *testing.T) {
	msg := &recordingMessaging{postReturnedTS: "1.0"}
	app := &Gateway{logger: newSilentLogger(), messaging: msg, alertCards: testAlertCards()}

	spec := configReloadingAlert()
	if _, _, err := app.postLifecycleAlert(context.Background(), "C1", "", spec); err != nil {
		t.Fatalf("postLifecycleAlert: %v", err)
	}

	want := statusMsgOptions(alertcard.NoticeText(spec))
	if msg.postOptionCount != len(want) {
		t.Fatalf("notice posted %d message options, want %d — it is not using the nudge helper",
			msg.postOptionCount, len(want))
	}
	// The notification text is the same single line, so a muted client still
	// shows something meaningful.
	if !strings.Contains(msg.postText, spec.Title) {
		t.Errorf("notice text %q does not carry the headline %q", msg.postText, spec.Title)
	}
}

// TestNoticeTextIsOneLine keeps a notice to the one thing it is for. Anything
// needing a reason, next steps or a diagnostic is an info card, and a caller
// that supplies them has picked the wrong level.
func TestNoticeTextIsOneLine(t *testing.T) {
	text := alertcard.NoticeText(alertcard.Spec{
		Level:     alertcard.LevelNotice,
		Title:     "Reloading the configuration…",
		Subtitle:  "The approved changes are being applied.",
		Reason:    "should not appear",
		NextSteps: "should not appear",
		Detail:    "should not appear",
	})
	if strings.Contains(text, "should not appear") {
		t.Errorf("notice carried card-only fields: %q", text)
	}
	if strings.Contains(text, "\n") {
		t.Errorf("notice is not a single line: %q", text)
	}
}
