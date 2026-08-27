package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

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
// the message the returning process edits. Both must render as the same info
// card, through the raw-blocks passthrough the container needs.
func TestRestartNoticeAndBackOnlineRenderAsInfoCards(t *testing.T) {
	poster := &fakeAlertAPI{}
	editor := &fakeAlertEditor{}
	app := &Gateway{
		logger:      newSilentLogger(),
		messaging:   &recordingMessaging{},
		alertCards:  testAlertCards(),
		alertAPI:    poster,
		alertEditor: editor,
	}

	channel, ts, err := app.postLifecycleAlert(context.Background(), "C1", "", restartNoticeAlert())
	if err != nil {
		t.Fatalf("postLifecycleAlert returned error: %v", err)
	}
	if len(poster.posts) != 1 {
		t.Fatalf("expected one card post, got %d", len(poster.posts))
	}
	assertInfoAlertCard(t, poster.posts[0].Blocks)

	if err := app.updateLifecycleAlert(context.Background(), channel, ts, backOnlineAlert()); err != nil {
		t.Fatalf("updateLifecycleAlert returned error: %v", err)
	}
	if len(editor.updates) != 1 {
		t.Fatalf("expected one card edit, got %d", len(editor.updates))
	}
	got := editor.updates[0]
	// The edit must land on the message the post created; that identity is what
	// makes the confirmation replace the notice instead of stacking below it.
	if got.ChannelID != channel || got.TS != ts {
		t.Fatalf("edit targeted %q/%q, want the posted message %q/%q", got.ChannelID, got.TS, channel, ts)
	}
	assertInfoAlertCard(t, got.Blocks)
	if !strings.Contains(got.Text, backOnlineAlert().Title) {
		t.Fatalf("expected the edit's notification text to carry the back-online headline, got %q", got.Text)
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
// failure — so none of them may carry a louder level.
func TestLifecycleAlertsAreAllInfo(t *testing.T) {
	for name, spec := range map[string]alertcard.Spec{
		"startup":       startupAlert(),
		"restartNotice": restartNoticeAlert(),
		"backOnline":    backOnlineAlert(),
		"pong":          pongAlert(),
	} {
		if spec.Level != alertcard.LevelInfo {
			t.Errorf("%s alert has level %q, want %q", name, spec.Level, alertcard.LevelInfo)
		}
		if strings.TrimSpace(spec.Title) == "" || strings.TrimSpace(spec.Subtitle) == "" {
			t.Errorf("%s alert needs both a title and a subtitle; they are the whole message while the card is collapsed", name)
		}
	}
}
