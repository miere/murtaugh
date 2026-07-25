package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

type fakeCanvasInfoAPI struct {
	channel *slack.Channel
	err     error
}

func (f fakeCanvasInfoAPI) GetConversationInfoContext(_ context.Context, _ *slack.GetConversationInfoInput) (*slack.Channel, error) {
	return f.channel, f.err
}

func TestSlackCanvasInfo_ResolvesFileID(t *testing.T) {
	ch := &slack.Channel{}
	ch.Properties = &slack.Properties{Canvas: slack.Canvas{FileId: "F0CANVAS"}}
	r := slackCanvasInfo{api: fakeCanvasInfoAPI{channel: ch}}

	got, err := r.ChannelCanvasFileID(context.Background(), "C1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "F0CANVAS" {
		t.Fatalf("canvas file id = %q, want F0CANVAS", got)
	}
}

func TestSlackCanvasInfo_NoCanvasIsEmptyNotPanic(t *testing.T) {
	r := slackCanvasInfo{api: fakeCanvasInfoAPI{channel: &slack.Channel{}}} // Properties nil
	got, err := r.ChannelCanvasFileID(context.Background(), "C1")
	if err != nil || got != "" {
		t.Fatalf("no-canvas channel = (%q, %v), want (\"\", nil)", got, err)
	}
}

func TestSlackCanvasInfo_PropagatesError(t *testing.T) {
	r := slackCanvasInfo{api: fakeCanvasInfoAPI{err: errors.New("slack down")}}
	if _, err := r.ChannelCanvasFileID(context.Background(), "C1"); err == nil {
		t.Fatal("expected the conversations.info error to surface")
	}
}

func TestPrependCanvasNote(t *testing.T) {
	// With a resolved id: the note names the id and precedes the history.
	got := prependCanvasNote("<thread-transcript>hi</thread-transcript>", "F0CANVAS")
	if !strings.Contains(got, "Slack canvas") || !strings.Contains(got, "F0CANVAS") {
		t.Fatalf("note should frame the canvas and name the id, got:\n%s", got)
	}
	if strings.Index(got, "canvas-context") > strings.Index(got, "thread-transcript") {
		t.Fatalf("canvas note should precede the transcript, got:\n%s", got)
	}
	// Without an id: still frames the surface, no id sentence, history preserved.
	noID := prependCanvasNote("HIST", "")
	if strings.Contains(noID, "canvas id") {
		t.Fatalf("no id should mean no id sentence, got:\n%s", noID)
	}
	if !strings.Contains(noID, "HIST") {
		t.Fatalf("history must be preserved, got:\n%s", noID)
	}
}

// TestBackfillWithSurface_DetectsCanvasRoot: a document_comment_root thread yields
// a CanvasContext anchored at the root ts; an ordinary thread yields nil.
func TestBackfillWithSurface_DetectsCanvasRoot(t *testing.T) {
	canvasRoot := slack.Message{Msg: slack.Msg{
		Timestamp: "1700000000.000100",
		User:      "USLACKBOT",
		SubType:   subtypeDocumentCommentRoot,
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			richTextBlock(&slack.RichTextSectionTextElement{Text: "a canvas section"}),
		}},
	}}
	api := &fakeBackfillAPI{
		replies: []slack.Message{canvasRoot, msg("1700000000.000200", "U1", "hi")},
		users:   map[string]*slack.User{"U1": userWithDisplayName("miere")},
	}
	b := NewThreadBackfiller(api, "UBOT", nil)

	_, canvas, err := b.BackfillWithSurface(context.Background(), "C1", "1700000000.000100", "1700000000.000999")
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if canvas == nil || canvas.SectionRef != "1700000000.000100" {
		t.Fatalf("expected a CanvasContext anchored at the root ts, got %+v", canvas)
	}

	// Ordinary thread → no canvas context.
	plain := &fakeBackfillAPI{
		replies: []slack.Message{msg("1700000000.000100", "U1", "hello")},
		users:   map[string]*slack.User{"U1": userWithDisplayName("miere")},
	}
	if _, c, err := NewThreadBackfiller(plain, "UBOT", nil).BackfillWithSurface(context.Background(), "C1", "1700000000.000100", "x"); err != nil || c != nil {
		t.Fatalf("ordinary thread should have nil canvas, got (%+v, %v)", c, err)
	}
}
