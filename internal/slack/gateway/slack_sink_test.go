package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh/internal/slack/alertcard"
)

var errSample = errors.New("boom")

// fakePoster captures buffered chat.postMessage calls.
type fakePoster struct {
	posts     []postedMessage
	postErr   error
	nextTS    int
	channelID string
}

type postedMessage struct {
	channelID string
	text      string
	threadTS  string
	blocks    string
}

func (p *fakePoster) PostMessageContext(_ context.Context, channelID string, options ...slack.MsgOption) (string, string, error) {
	if p.postErr != nil {
		return "", "", p.postErr
	}
	// Compose the options exactly as slack-go would, then read back the fields
	// these tests assert on from the resulting request values.
	_, values, err := slack.UnsafeApplyMsgOptions("token", channelID, "https://slack.test/api/", options...)
	if err != nil {
		return "", "", err
	}
	p.posts = append(p.posts, postedMessage{
		channelID: channelID,
		text:      values.Get("text"),
		threadTS:  values.Get("thread_ts"),
		blocks:    values.Get("blocks"),
	})
	p.nextTS++
	return channelID, "ts-" + strings.Repeat("x", p.nextTS), nil
}

func canvasAPI() *fakeStreamAPI {
	return &fakeStreamAPI{startErr: slack.SlackErrorResponse{Err: slackChannelTypeUnsupported}}
}

// TestDefaultSlackSink_StreamsWhenSupported: on an ordinary surface the sink
// streams and never posts.
func TestDefaultSlackSink_StreamsWhenSupported(t *testing.T) {
	api := &fakeStreamAPI{}
	poster := &fakePoster{}
	s := newDefaultSlackSink(api, poster, "C1", StreamWriterOptions{ThreadTS: "100.0", MinChars: 1, Logger: discardLogger()}, discardLogger())
	ctx := context.Background()

	if err := s.Append(ctx, "hello world"); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if api.starts != 1 || api.appends == 0 {
		t.Fatalf("expected a stream (starts=1, appends>0), got starts=%d appends=%d", api.starts, api.appends)
	}
	if len(poster.posts) != 0 {
		t.Fatalf("expected no buffered posts on a streamable surface, got %d", len(poster.posts))
	}
}

// TestDefaultSlackSink_DowngradesOnCanvas: a surface that rejects the stream with
// channel_type_not_supported downgrades once to a buffered post carrying the full
// reply — the #87 fix.
func TestDefaultSlackSink_DowngradesOnCanvas(t *testing.T) {
	api := canvasAPI()
	poster := &fakePoster{}
	s := newDefaultSlackSink(api, poster, "C1", StreamWriterOptions{ThreadTS: "100.0", MinChars: 1, Logger: discardLogger()}, discardLogger())
	ctx := context.Background()

	if err := s.Append(ctx, "part one. "); err != nil {
		t.Fatalf("Append 1: %v", err)
	}
	if err := s.Append(ctx, "part two."); err != nil {
		t.Fatalf("Append 2: %v", err)
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if api.starts != 1 {
		t.Fatalf("expected exactly one stream-start attempt before downgrade, got %d", api.starts)
	}
	if api.appends != 0 {
		t.Fatalf("expected no stream appends after downgrade, got %d", api.appends)
	}
	if len(poster.posts) != 1 {
		t.Fatalf("expected one buffered post, got %d", len(poster.posts))
	}
	if got := poster.posts[0].text; got != "part one. part two." {
		t.Fatalf("buffered post text = %q, want the full reply", got)
	}
	if got := poster.posts[0].threadTS; got != "100.0" {
		t.Fatalf("buffered post thread_ts = %q, want 100.0", got)
	}
}

// TestDefaultSlackSink_AlertTextDowngrades: when an alert cannot be delivered as
// a card it is painted on the reply surface instead (sectionRenderer.postAlert),
// and on a canvas that paint must downgrade to a buffered post rather than
// erroring the stream-open a second time. This is the last line of defence on a
// turn that has already failed, so it has to land.
func TestDefaultSlackSink_AlertTextDowngrades(t *testing.T) {
	api := canvasAPI()
	poster := &fakePoster{}
	s := newDefaultSlackSink(api, poster, "C1", StreamWriterOptions{MinChars: 1, Logger: discardLogger()}, discardLogger())

	ctx := context.Background()
	if err := s.Append(ctx, alertcard.PlainText(failSpec(errSample))); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(poster.posts) != 1 {
		t.Fatalf("expected the alert posted once, got %d posts", len(poster.posts))
	}
	if !strings.Contains(poster.posts[0].text, "hit an error") || !strings.Contains(poster.posts[0].text, "boom") {
		t.Fatalf("alert post = %q, want the notice + cause", poster.posts[0].text)
	}
}

// TestDefaultSlackSink_TransientErrorNotDowngraded: a non-canvas stream error is a
// real failure — it must surface, not silently downgrade to buffered.
func TestDefaultSlackSink_TransientErrorNotDowngraded(t *testing.T) {
	api := &fakeStreamAPI{startErr: slack.SlackErrorResponse{Err: "ratelimited"}}
	poster := &fakePoster{}
	s := newDefaultSlackSink(api, poster, "C1", StreamWriterOptions{MinChars: 1, Logger: discardLogger()}, discardLogger())

	err := s.Append(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected the transient stream error to surface")
	}
	if len(poster.posts) != 0 {
		t.Fatalf("expected NO buffered fallback on a transient error, got %d posts", len(poster.posts))
	}
}

// TestBufferedSink_EmptyRunPostsNothing: a text section that buffered nothing must
// not post an empty message (matches streaming's no-empty-message property).
func TestBufferedSink_EmptyRunPostsNothing(t *testing.T) {
	poster := &fakePoster{}
	b := newBufferedSink(poster, "C1", StreamWriterOptions{Logger: discardLogger()})
	if err := b.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(poster.posts) != 0 {
		t.Fatalf("expected no post for an empty run, got %d", len(poster.posts))
	}
	if b.Started() {
		t.Fatal("Started() should be false when nothing was appended")
	}
}

// TestBufferedSink_ChunksLongReply: a reply over the single-message limit is split
// into ordered posts and never truncated.
func TestBufferedSink_ChunksLongReply(t *testing.T) {
	poster := &fakePoster{}
	b := newBufferedSink(poster, "C1", StreamWriterOptions{Logger: discardLogger()})
	// Build a > limit reply as space-separated words so it splits on boundaries.
	long := strings.TrimSpace(strings.Repeat("word ", maxBufferedPostChars)) // ~5x the limit in chars
	if err := b.Append(context.Background(), long); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := b.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(poster.posts) < 2 {
		t.Fatalf("expected the long reply split across multiple posts, got %d", len(poster.posts))
	}
	var reassembled strings.Builder
	for _, p := range poster.posts {
		if n := len([]rune(p.text)); n > maxBufferedPostChars {
			t.Fatalf("a chunk exceeded the limit: %d runes", n)
		}
		reassembled.WriteString(strings.TrimSpace(p.text))
		reassembled.WriteString(" ")
	}
	// No words dropped: rune count of the words survives (whitespace normalised).
	wantWords := strings.Fields(long)
	gotWords := strings.Fields(reassembled.String())
	if len(gotWords) != len(wantWords) {
		t.Fatalf("word count changed across chunking: got %d, want %d", len(gotWords), len(wantWords))
	}
}

func TestSplitForSlack_ShortIsSinglePiece(t *testing.T) {
	parts := splitForSlack("short reply", maxBufferedPostChars)
	if len(parts) != 1 || parts[0] != "short reply" {
		t.Fatalf("splitForSlack short = %v, want single unchanged piece", parts)
	}
}
