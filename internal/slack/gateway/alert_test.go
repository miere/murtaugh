package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/slack/alertcard"
	slackclient "github.com/miere/murtaugh/internal/slack/client"
)

// fakeAlertAPI captures the raw-block posts an alert makes.
type fakeAlertAPI struct {
	posts   []slackclient.PostMessageParams
	postErr error
}

func (f *fakeAlertAPI) PostMessage(_ context.Context, p slackclient.PostMessageParams) (slackclient.PostMessageResult, error) {
	if f.postErr != nil {
		return slackclient.PostMessageResult{}, f.postErr
	}
	f.posts = append(f.posts, p)
	return slackclient.PostMessageResult{Channel: p.ChannelID, TS: "1.0"}, nil
}

func testAlertCards() *alertcard.Renderer { return alertcard.NewRenderer("", assets.FS) }

// alertRenderer wires a sectionRenderer whose alerts go to api. A nil api leaves
// the poster unwired, which is the text-fallback path.
func alertRenderer(stream *fakeStreamAPI, msgr *fakeStatusMessenger, api alertMessagePoster) *sectionRenderer {
	var poster alertPoster
	if api != nil {
		poster = newAlertPoster(api, testAlertCards(), "C1", "100.0")
	}
	return newSectionRenderer(
		func() SlackSink {
			return NewStreamWriter(stream, "C1", StreamWriterOptions{ThreadTS: "100.0", Interval: time.Hour, MinChars: 1, Logger: discardLogger()})
		},
		func() toolBlock {
			return NewStatusLineWriter(msgr, "C1", "100.0", time.Hour, discardLogger())
		},
		nil, poster, "C1", "100.0",
		discardLogger(),
	)
}

// streamedText concatenates everything painted on the reply surface.
func streamedText(t *testing.T, api *fakeStreamAPI) string {
	t.Helper()
	var out string
	for _, opts := range append(api.startOptions, api.appendOptions...) {
		if text, err := extractMarkdownTextFromOptions(opts...); err == nil {
			out += text
		}
	}
	return out
}

// A failure is its own message below the reply, not text appended into it. That
// is the whole reason it can be a collapsed card: a container block cannot live
// inside an in-flight stream.
func TestSectionRendererFailPostsACardNotStreamText(t *testing.T) {
	stream, msgr, api := &fakeStreamAPI{}, &fakeStatusMessenger{}, &fakeAlertAPI{}
	r := alertRenderer(stream, msgr, api)
	ctx := context.Background()

	_ = r.Text(ctx, "here is what I found")
	if err := r.Fail(ctx, errors.New("acp: session terminated")); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	if len(api.posts) != 1 {
		t.Fatalf("want one alert card posted, got %d", len(api.posts))
	}
	post := api.posts[0]
	if post.ThreadTS != "100.0" || post.ChannelID != "C1" {
		t.Errorf("alert posted to %s/%s, want C1/100.0", post.ChannelID, post.ThreadTS)
	}
	// The notification form, so a push notification is not "content unavailable".
	if !strings.Contains(post.Text, "Oops!") {
		t.Errorf("fallback text = %q, want the headline", post.Text)
	}
	if !strings.Contains(string(post.Blocks), `"type": "container"`) {
		t.Errorf("alert did not post a container block: %s", post.Blocks)
	}
	if !strings.Contains(string(post.Blocks), "acp: session terminated") {
		t.Errorf("alert card dropped the diagnostic: %s", post.Blocks)
	}

	// The reply keeps the agent's own words and nothing else.
	if got := streamedText(t, stream); got != "here is what I found" {
		t.Errorf("reply surface = %q, want the agent's text alone", got)
	}
}

// The reply must be sealed before the card lands, or the alert interleaves with
// an unfinished stream.
func TestSectionRendererFailSealsTheReplyFirst(t *testing.T) {
	stream, msgr, api := &fakeStreamAPI{}, &fakeStatusMessenger{}, &fakeAlertAPI{}
	r := alertRenderer(stream, msgr, api)
	ctx := context.Background()

	_ = r.Text(ctx, "partial answer")
	_ = r.Fail(ctx, errors.New("boom"))

	if stream.stops == 0 {
		t.Error("the open reply section was not sealed before the alert posted")
	}
}

// With no Slack client wired the alert still has to reach the user — these fire
// on paths where something has already gone wrong.
func TestSectionRendererFailFallsBackToText(t *testing.T) {
	stream, msgr := &fakeStreamAPI{}, &fakeStatusMessenger{}
	r := alertRenderer(stream, msgr, nil)
	ctx := context.Background()

	_ = r.Fail(ctx, errors.New("acp: session terminated"))

	got := streamedText(t, stream)
	if !strings.Contains(got, "hit an error while talking to the agent") {
		t.Errorf("reply surface = %q, want the alert painted as text", got)
	}
	if !strings.Contains(got, "acp: session terminated") {
		t.Errorf("text fallback dropped the cause: %q", got)
	}
}

// A post that fails takes the same fallback — this is what covers a surface that
// turns out not to accept Block Kit.
func TestSectionRendererFailFallsBackWhenThePostFails(t *testing.T) {
	stream, msgr := &fakeStreamAPI{}, &fakeStatusMessenger{}
	api := &fakeAlertAPI{postErr: errors.New("invalid_blocks")}
	r := alertRenderer(stream, msgr, api)
	ctx := context.Background()

	_ = r.Fail(ctx, errors.New("boom"))

	if got := streamedText(t, stream); !strings.Contains(got, "hit an error") {
		t.Errorf("reply surface = %q, want the alert painted as text after the post failed", got)
	}
}

// A card that failed but whose text landed is still a delivered alert, so Fail
// reports success — the message arrived, just plainer.
func TestSectionRendererFailSucceedsWhenOnlyTheTextLands(t *testing.T) {
	stream, msgr := &fakeStreamAPI{}, &fakeStatusMessenger{}
	api := &fakeAlertAPI{postErr: errors.New("invalid_blocks")}
	r := alertRenderer(stream, msgr, api)

	if err := r.Fail(context.Background(), errors.New("boom")); err != nil {
		t.Errorf("Fail = %v, want nil when the text fallback delivered", err)
	}
}

// Both routes failing is the one case the caller must hear about: the user was
// told nothing at all.
func TestSectionRendererFailReportsWhenNothingReachedTheUser(t *testing.T) {
	stream := &fakeStreamAPI{startErr: errors.New("slack down")}
	api := &fakeAlertAPI{postErr: errors.New("invalid_blocks")}
	r := alertRenderer(stream, &fakeStatusMessenger{}, api)

	if err := r.Fail(context.Background(), errors.New("boom")); err == nil {
		t.Error("Fail = nil, want an error when neither the card nor the text landed")
	}
}

// The guarantee 42a6ef7 was written to protect, carried into this design: an
// alert lands even when the reply stream it follows cannot be sealed.
//
// That commit fixed it inside StreamWriter.Fail, by dropping the pending text
// before painting the notice — because emit RETAINS pending on error, and the
// pending text is usually why the stream is failing, so painting on top of it
// re-sent the same rejected bytes and lost the reply, the notice and the error
// together. This design reaches the same place differently: the alert is its own
// message, and closeText drops the unsealable sink so the fallback opens a fresh
// one instead of inheriting the refused buffer. Either way the user hears about
// the failure — which is the part that must not regress.
func TestAlertLandsWhenTheReplyStreamCannotBeSealed(t *testing.T) {
	stream, msgr, api := &fakeStreamAPI{}, &fakeStatusMessenger{}, &fakeAlertAPI{}
	r := alertRenderer(stream, msgr, api)
	ctx := context.Background()

	_ = r.Text(ctx, "a reply that Slack will refuse to finalize")
	// Slack has finalized the stream out from under us: append and stop both
	// fail until a fresh message is opened.
	stream.finalizeUntilStart = true

	if err := r.Fail(ctx, errors.New("boom")); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if len(api.posts) != 1 {
		t.Fatalf("the alert did not land, got %d posts", len(api.posts))
	}
}

// Same guarantee with no card poster wired, so the alert has to go out as text
// on a sink that has just refused to seal. It must open a fresh stream rather
// than re-send the rejected bytes.
func TestAlertTextLandsOnAFreshStreamAfterAFailedSeal(t *testing.T) {
	stream, msgr := &fakeStreamAPI{}, &fakeStatusMessenger{}
	r := alertRenderer(stream, msgr, nil)
	ctx := context.Background()

	_ = r.Text(ctx, "a reply that Slack will refuse to finalize")
	stream.finalizeUntilStart = true
	startsBefore := stream.starts

	if err := r.Fail(ctx, errors.New("boom")); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if stream.starts <= startsBefore {
		t.Error("the alert reused the refused stream instead of opening a fresh one")
	}
	if got := streamedText(t, stream); !strings.Contains(got, "hit an error") {
		t.Errorf("reply surface = %q, want the alert text", got)
	}
}

// A turn that replied normally posts no alert at all.
func TestSectionRendererFinishPostsNoAlertOnASuccessfulTurn(t *testing.T) {
	stream, msgr, api := &fakeStreamAPI{}, &fakeStatusMessenger{}, &fakeAlertAPI{}
	r := alertRenderer(stream, msgr, api)
	ctx := context.Background()

	_ = r.Text(ctx, "all done")
	if err := r.Finish(ctx, nil); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if len(api.posts) != 0 {
		t.Fatalf("want no alert on a successful turn, got %d", len(api.posts))
	}
}

func TestSectionRendererFinishPostsTheEmptyReplyCard(t *testing.T) {
	stream, msgr, api := &fakeStreamAPI{}, &fakeStatusMessenger{}, &fakeAlertAPI{}
	r := alertRenderer(stream, msgr, api)
	ctx := context.Background()

	spec := emptyReplySpec(2)
	if err := r.Finish(ctx, &spec); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if len(api.posts) != 1 {
		t.Fatalf("want the empty-reply card posted, got %d", len(api.posts))
	}
	blocks := string(api.posts[0].Blocks)
	if !strings.Contains(blocks, "The agent ran 2 tools and finished without a reply.") {
		t.Errorf("card does not state what the turn did: %s", blocks)
	}
	// It is a warning, so it carries the warn icon rather than the error one.
	if !strings.Contains(blocks, "danger-electrician") {
		t.Errorf("empty-reply card is not using the warn icon: %s", blocks)
	}
}

// Every alert must arrive folded — that is the fix for the intrusive walls of
// error text this replaced.
func TestAlertCardsArriveCollapsed(t *testing.T) {
	stream, msgr, api := &fakeStreamAPI{}, &fakeStatusMessenger{}, &fakeAlertAPI{}
	r := alertRenderer(stream, msgr, api)
	ctx := context.Background()

	_ = r.Fail(ctx, errors.New("boom"))

	var doc struct {
		Blocks []struct {
			IsCollapsible   bool `json:"is_collapsible"`
			DefaultCollapse bool `json:"default_collapsed"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(api.posts[0].Blocks, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !doc.Blocks[0].IsCollapsible || !doc.Blocks[0].DefaultCollapse {
		t.Error("alert card did not arrive collapsed")
	}
}
