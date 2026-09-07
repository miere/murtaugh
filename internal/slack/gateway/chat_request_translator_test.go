package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/slack-go/slack"
)

// fakeBackfiller stands in for the Slack thread reader: it returns a canned
// transcript (and optionally a canvas surface), or an error, and records that it
// was consulted at all — which is what the warm-session case asserts.
type fakeBackfiller struct {
	history string
	canvas  *CanvasContext
	err     error
	calls   int
}

func (b *fakeBackfiller) BackfillWithSurface(context.Context, string, string, string) (string, *CanvasContext, error) {
	b.calls++
	return b.history, b.canvas, b.err
}

// fakeCanvasInfo stands in for conversations.info.
type fakeCanvasInfo struct {
	fileID string
	err    error
}

func (c *fakeCanvasInfo) ChannelCanvasFileID(context.Context, string) (string, error) {
	return c.fileID, c.err
}

// warmSessions reports every conversation as already having a live session.
type warmSessions struct{}

func (warmSessions) Lookup(agent.ConversationKey) (string, bool) { return "sess-1", true }

// foldNothing is the no-uploads case; foldFixed folds a fixed block so a test can
// see where it lands without wiring a file fetcher.
func foldFixed(block string) func(context.Context, []slack.File) string {
	return func(context.Context, []slack.File) string { return block }
}

// TestRequestTranslatorBindsTheConversationAndTheReply is the routing half: the
// session key and the place the reply is posted must be derived from ONE
// decision, or a turn answers in a thread whose session it is not bound to. The
// channel-reply row is the one that matters — an empty thread there is the
// strategy (one rolling channel-wide conversation), not a missing value.
func TestRequestTranslatorBindsTheConversationAndTheReply(t *testing.T) {
	for _, tc := range []struct {
		name          string
		req           ChatRequest
		replyOnThread bool
		wantKeyThread string
		wantReplyTS   string
	}{
		{
			name:          "DM roots a thread at its own message",
			req:           ChatRequest{ChannelID: "D1", MessageTS: "100.0", Text: "hi", DM: true},
			replyOnThread: true,
			wantKeyThread: "100.0",
			wantReplyTS:   "100.0",
		},
		{
			name:          "a reply inside a thread stays in that thread",
			req:           ChatRequest{ChannelID: "C1", ThreadTS: "100.0", MessageTS: "101.0", Text: "hi"},
			replyOnThread: false,
			wantKeyThread: "100.0",
			wantReplyTS:   "100.0",
		},
		{
			name:          "channel-reply mode shares one channel-wide conversation",
			req:           ChatRequest{ChannelID: "C1", MessageTS: "100.0", Text: "hi"},
			replyOnThread: false,
			wantKeyThread: "",
			wantReplyTS:   "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newRequestTranslator(nil, nil, nil, discardLogger())
			got, err := tr.Translate(context.Background(), tc.req, ChatRoute{Agent: "default", ReplyOnThread: tc.replyOnThread}, nil)
			if err != nil {
				t.Fatalf("Translate: %v", err)
			}
			if got.Key.ThreadTS != tc.wantKeyThread {
				t.Errorf("session key thread = %q, want %q", got.Key.ThreadTS, tc.wantKeyThread)
			}
			if got.ReplyThreadTS != tc.wantReplyTS {
				t.Errorf("reply thread = %q, want %q", got.ReplyThreadTS, tc.wantReplyTS)
			}
			// The metadata must agree with the key, not with the raw request:
			// this is the value the agent (and a node) sees as "the thread".
			if got.Metadata.ThreadTS != got.Key.ThreadTS {
				t.Errorf("metadata thread %q disagrees with the session key %q", got.Metadata.ThreadTS, got.Key.ThreadTS)
			}
		})
	}
}

// TestRequestTranslatorFoldsUploadsIntoThePrompt covers the three shapes a
// message with files takes. The caption-less upload is the interesting one: it is
// a valid prompt even though the person typed nothing, and rejecting it would
// make dropping a file into a DM do nothing at all.
func TestRequestTranslatorFoldsUploadsIntoThePrompt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		text       string
		uploads    string
		wantPrompt string
		wantUser   string
	}{
		{name: "text only", text: "  look at this  ", uploads: "", wantPrompt: "look at this", wantUser: "look at this"},
		{name: "files only", text: "", uploads: "<file/>", wantPrompt: "<file/>", wantUser: ""},
		{name: "both", text: "look", uploads: "<file/>", wantPrompt: "look\n\n<file/>", wantUser: "look"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newRequestTranslator(nil, nil, foldFixed(tc.uploads), discardLogger())
			req := ChatRequest{ChannelID: "C1", MessageTS: "100.0", Text: tc.text}
			got, err := tr.Translate(context.Background(), req, ChatRoute{Agent: "default", ReplyOnThread: true}, nil)
			if err != nil {
				t.Fatalf("Translate: %v", err)
			}
			if got.Prompt.Text != tc.wantPrompt {
				t.Errorf("prompt = %q, want %q", got.Prompt.Text, tc.wantPrompt)
			}
			// The journal records what the person wrote, not what the file said.
			if got.UserText != tc.wantUser {
				t.Errorf("user text = %q, want %q", got.UserText, tc.wantUser)
			}
		})
	}
}

// TestRequestTranslatorRejectsTurnsItCannotRun covers the only two failures this
// direction has, both sentinels so a caller can tell them apart without matching
// prose. The timestamp guard is checked against the TRIGGERING message rather
// than the reply thread, because the reply thread is legitimately empty in
// channel-reply mode.
func TestRequestTranslatorRejectsTurnsItCannotRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		req     ChatRequest
		uploads string
		want    error
	}{
		{
			name: "nothing said and nothing attached",
			req:  ChatRequest{ChannelID: "C1", MessageTS: "100.0", Text: "   "},
			want: errEmptyPrompt,
		},
		{
			name: "no Slack timestamp to post against",
			req:  ChatRequest{ChannelID: "C1", Text: "hi"},
			want: errNoSourceTimestamp,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newRequestTranslator(nil, nil, foldFixed(tc.uploads), discardLogger())
			if _, err := tr.Translate(context.Background(), tc.req, ChatRoute{Agent: "default", ReplyOnThread: true}, nil); !errors.Is(err, tc.want) {
				t.Fatalf("Translate error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestRequestTranslatorSeedsAColdThreadOnce: a cold threaded session gets the
// prior conversation as history, and a warm one does not — it already holds it,
// and re-sending would duplicate the whole thread into the model's context every
// turn.
func TestRequestTranslatorSeedsAColdThreadOnce(t *testing.T) {
	req := ChatRequest{ChannelID: "C1", ThreadTS: "100.0", MessageTS: "101.0", Text: "carry on"}
	route := ChatRoute{Agent: "default", ReplyOnThread: true}

	cold := &fakeBackfiller{history: "U1: earlier"}
	got, err := newRequestTranslator(cold, nil, nil, discardLogger()).Translate(context.Background(), req, route, nil)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got.Prompt.History != "U1: earlier" {
		t.Errorf("a cold thread must be seeded with its history, got %q", got.Prompt.History)
	}

	warm := &fakeBackfiller{history: "U1: earlier"}
	got, err = newRequestTranslator(warm, nil, nil, discardLogger()).Translate(context.Background(), req, route, warmSessions{})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if warm.calls != 0 || got.Prompt.History != "" {
		t.Errorf("a warm session must not be re-seeded: calls=%d history=%q", warm.calls, got.Prompt.History)
	}
}

// TestRequestTranslatorDegradesWhenTheThreadCannotBeRead: losing the backstory is
// worse than the turn failing only if you value completeness over answering at
// all. A failed replies fetch proceeds without history.
func TestRequestTranslatorDegradesWhenTheThreadCannotBeRead(t *testing.T) {
	b := &fakeBackfiller{err: errors.New("ratelimited")}
	tr := newRequestTranslator(b, nil, nil, discardLogger())
	req := ChatRequest{ChannelID: "C1", ThreadTS: "100.0", MessageTS: "101.0", Text: "carry on"}

	got, err := tr.Translate(context.Background(), req, ChatRoute{Agent: "default", ReplyOnThread: true}, nil)
	if err != nil {
		t.Fatalf("a failed backfill must not fail the turn: %v", err)
	}
	if got.Prompt.History != "" || got.Prompt.Text != "carry on" {
		t.Errorf("expected the prompt to survive without history, got %+v", got.Prompt)
	}
}

// TestRequestTranslatorCanvasTurnCarriesTheDocument: a canvas comment turn must
// tell the agent it is looking at a canvas AND which one, ahead of the transcript
// so the model reads the framing before the content (spec 021 §9.3).
func TestRequestTranslatorCanvasTurnCarriesTheDocument(t *testing.T) {
	b := &fakeBackfiller{history: "U1: what does this say?", canvas: &CanvasContext{SectionRef: "100.0"}}
	tr := newRequestTranslator(b, &fakeCanvasInfo{fileID: "F123"}, nil, discardLogger())
	req := ChatRequest{ChannelID: "C1", ThreadTS: "100.0", MessageTS: "101.0", Text: "summarise"}

	got, err := tr.Translate(context.Background(), req, ChatRoute{Agent: "default", ReplyOnThread: true}, nil)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got.Metadata.Surface != "canvas" || got.Metadata.CanvasID != "F123" {
		t.Fatalf("canvas surface not recorded: %+v", got.Metadata)
	}
	if !strings.HasPrefix(got.Prompt.History, "<canvas-context>") {
		t.Errorf("the canvas framing must precede the transcript, got %q", got.Prompt.History)
	}
	if !strings.Contains(got.Prompt.History, "F123") || !strings.Contains(got.Prompt.History, "U1: what does this say?") {
		t.Errorf("the canvas note must carry the id and keep the transcript, got %q", got.Prompt.History)
	}
}

// TestRequestTranslatorCanvasTurnSurvivesAnUnresolvedID: knowing the turn came
// from a canvas is worth more than the id, so a conversations.info failure must
// not cost the surface — and must not fail the turn.
func TestRequestTranslatorCanvasTurnSurvivesAnUnresolvedID(t *testing.T) {
	b := &fakeBackfiller{canvas: &CanvasContext{SectionRef: "100.0"}}
	tr := newRequestTranslator(b, &fakeCanvasInfo{err: errors.New("channel_not_found")}, nil, discardLogger())
	req := ChatRequest{ChannelID: "C1", ThreadTS: "100.0", MessageTS: "101.0", Text: "summarise"}

	got, err := tr.Translate(context.Background(), req, ChatRoute{Agent: "default", ReplyOnThread: true}, nil)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got.Metadata.Surface != "canvas" || got.Metadata.CanvasID != "" {
		t.Fatalf("expected the surface without an id, got %+v", got.Metadata)
	}
	if !strings.Contains(got.Prompt.History, "<canvas-context>") {
		t.Errorf("the framing must still be sent, got %q", got.Prompt.History)
	}
}
