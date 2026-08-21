package gateway

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// paragraphs builds n paragraphs of ASCII prose, so character counts and byte
// counts agree and a test can reason about the budget in either.
func paragraphs(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("Lorem ipsum dolor sit amet, consectetur adipiscing elit, sed do eiusmod tempor.\n\n")
	}
	return b.String()
}

// budgetedWriter pairs a writer with a fake Slack that enforces the real
// per-message character cap, so these tests fail the same way production did.
func budgetedWriter(t *testing.T) (*fakeStreamAPI, *StreamWriter) {
	t.Helper()
	api := &fakeStreamAPI{charBudget: maxStreamMessageChars}
	return api, NewStreamWriter(api, "C1", StreamWriterOptions{Interval: time.Hour, MinChars: 1})
}

// TestStreamWriterRollsOverWhenMessageIsFull is the regression test for the
// silent-noop bug. A reply larger than one streaming message must span several,
// losing nothing — not fail, and not vanish.
//
// The cap is per *message*, not per append, so splitting the appends alone would
// not save this: the fake enforces the total exactly as Slack does.
func TestStreamWriterRollsOverWhenMessageIsFull(t *testing.T) {
	api, writer := budgetedWriter(t)
	ctx := context.Background()
	reply := paragraphs(400) // ~32k chars: three messages' worth

	if err := writer.Append(ctx, reply); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	messages := api.messageTexts()
	if len(messages) < 3 {
		t.Fatalf("expected the reply to span at least 3 messages, got %d", len(messages))
	}
	for i, msg := range messages {
		if n := utf8.RuneCountInString(msg); n > maxStreamMessageChars {
			t.Fatalf("message %d holds %d chars, over Slack's %d cap", i, n, maxStreamMessageChars)
		}
	}
	if got := strings.Join(messages, ""); got != reply {
		t.Fatalf("reassembled reply differs from the original (%d chars vs %d)",
			utf8.RuneCountInString(got), utf8.RuneCountInString(reply))
	}
}

// TestStreamWriterRollsOverOnParagraphBoundary proves the split lands where a
// reader would put it. Slack concatenates appends within a message, so the cut
// only shows at a rollover — which is exactly where it must not fall mid-sentence.
func TestStreamWriterRollsOverOnParagraphBoundary(t *testing.T) {
	api, writer := budgetedWriter(t)
	ctx := context.Background()
	if err := writer.Append(context.Background(), paragraphs(300)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	messages := api.messageTexts()
	if len(messages) < 2 {
		t.Fatalf("expected a rollover, got %d message(s)", len(messages))
	}
	for i, msg := range messages[:len(messages)-1] {
		if !strings.HasSuffix(msg, "\n\n") {
			tail := msg[max(0, len(msg)-40):]
			t.Fatalf("message %d ends mid-paragraph (tail %q); the cut should land on a blank line", i, tail)
		}
	}
}

// TestStreamWriterKeepsCodeFenceIntactAcrossRollover proves a code block that
// outgrows one message stays a code block. Without the fence carry, the first
// message ends with an unterminated ``` and the second opens with raw backticked
// prose — which is what every long reply containing code would now look like,
// since a size budget makes rollover routine rather than rare.
func TestStreamWriterKeepsCodeFenceIntactAcrossRollover(t *testing.T) {
	api, writer := budgetedWriter(t)
	ctx := context.Background()

	var code strings.Builder
	code.WriteString("Here is the file:\n\n```go\n")
	for i := 0; i < 900; i++ {
		code.WriteString("\tfmt.Println(\"a line of code that is not especially short\")\n")
	}
	code.WriteString("```\n")

	if err := writer.Append(ctx, code.String()); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := writer.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	messages := api.messageTexts()
	if len(messages) < 2 {
		t.Fatalf("expected the code block to span messages, got %d", len(messages))
	}
	for i, msg := range messages {
		if n := strings.Count(msg, "```"); n%2 != 0 {
			t.Fatalf("message %d has %d fence delimiters — an odd count means a broken code block", i, n)
		}
	}
	if !strings.Contains(messages[1], "```go") {
		t.Fatalf("the continuation message should reopen the fence with its language, got prefix %q",
			messages[1][:min(40, len(messages[1]))])
	}
}

// TestStreamWriterRecoversFromUnexpectedTooLong proves the reactive net works
// when the pre-emptive budget is wrong — i.e. when Slack moves the number or
// counts something we do not. The writer must roll over and land the text, not
// surface msg_too_long as a turn failure.
func TestStreamWriterRecoversFromUnexpectedTooLong(t *testing.T) {
	// A budget far below what the writer believes it has, so the writer's own
	// accounting never triggers a rollover and only the error path can save it.
	api := &fakeStreamAPI{charBudget: 200}
	writer := NewStreamWriter(api, "C1", StreamWriterOptions{Interval: time.Hour, MinChars: 1})
	ctx := context.Background()

	first := strings.Repeat("a", 150) + "\n"
	second := strings.Repeat("b", 150) + "\n"
	if err := writer.Append(ctx, first); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := writer.Append(ctx, second); err != nil {
		t.Fatalf("second append should recover via rollover, got: %v", err)
	}
	if err := writer.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if api.starts < 2 {
		t.Fatalf("expected a rollover onto a second message, got %d start(s)", api.starts)
	}
	if got := strings.Join(api.messageTexts(), ""); got != first+second {
		t.Fatalf("text lost across the reactive rollover: got %q", got)
	}
}

// TestAlertLandsOverRejectedReplyText is the other half of the silent-noop
// regression. When the buffered reply is itself what Slack refuses, the failure
// alert must still land: the user's only signal that the turn died cannot be
// held hostage by the content that killed it.
//
// It was written against StreamWriter.Fail, which fixed this by dropping the
// retained text before painting the notice into the same message. The alert card
// changed the seam — a failure is now its own message, and closeText drops the
// unsealable sink so the alert opens a fresh one — but the regression it guards
// is unchanged, so the test moved down to the renderer rather than going away.
//
// The text path is asserted here rather than the card path because it is the
// harder one: the card goes out through a different client and never touches the
// poisoned stream at all.
func TestAlertLandsOverRejectedReplyText(t *testing.T) {
	// A message cap low enough that no rollover can rescue the reply, but high
	// enough for the alert itself: this reproduces the production shape, where
	// the buffered text is unpaintable *anywhere* and therefore stays in the
	// buffer no matter how many times it is retried.
	api := &fakeStreamAPI{charBudget: 400}
	r := alertRenderer(api, &fakeStatusMessenger{}, nil)
	ctx := context.Background()

	if err := r.Text(ctx, strings.Repeat("x", 500)); err == nil {
		t.Fatalf("expected the oversized reply to surface an error")
	}

	if err := r.Fail(ctx, context.DeadlineExceeded); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	painted := strings.Join(api.messageTexts(), "")
	if !strings.Contains(painted, "hit an error") {
		t.Fatalf("failure alert never landed; painted %q", painted)
	}
	if api.stops == 0 {
		t.Fatalf("the alert must seal its message; leaving it streaming is the empty-bubble symptom")
	}
}

func TestSplitAtBudget(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		room      int
		wantPiece string
		wantRest  string
	}{
		{
			name:      "fits whole",
			text:      "short",
			room:      100,
			wantPiece: "short",
			wantRest:  "",
		},
		{
			name:      "prefers a paragraph break over a line break",
			text:      "one\ntwo\n\nthree\nfour",
			room:      15,
			wantPiece: "one\ntwo\n\n",
			wantRest:  "three\nfour",
		},
		{
			name:      "falls back to a line break",
			text:      "alpha\nbravo\ncharlie",
			room:      13,
			wantPiece: "alpha\nbravo\n",
			wantRest:  "charlie",
		},
		{
			name:      "falls back to a word break",
			text:      "alpha bravo charlie",
			room:      13,
			wantPiece: "alpha bravo ",
			wantRest:  "charlie",
		},
		{
			name:      "hard cuts an unbroken run",
			text:      strings.Repeat("x", 20),
			room:      8,
			wantPiece: strings.Repeat("x", 8),
			wantRest:  strings.Repeat("x", 12),
		},
		{
			// The cap counts characters, so a hard cut must land on a rune
			// boundary — slicing bytes here would emit half an em-dash.
			name:      "hard cuts multibyte text on a rune boundary",
			text:      strings.Repeat("—", 10),
			room:      4,
			wantPiece: strings.Repeat("—", 4),
			wantRest:  strings.Repeat("—", 6),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			piece, rest := splitAtBudget(tc.text, tc.room)
			if piece != tc.wantPiece {
				t.Fatalf("piece = %q, want %q", piece, tc.wantPiece)
			}
			if rest != tc.wantRest {
				t.Fatalf("rest = %q, want %q", rest, tc.wantRest)
			}
			if piece+rest != tc.text {
				t.Fatalf("split lost text: %q + %q != %q", piece, rest, tc.text)
			}
			if n := utf8.RuneCountInString(piece); n > tc.room {
				t.Fatalf("piece is %d chars, over the %d room", n, tc.room)
			}
			if !utf8.ValidString(piece) || !utf8.ValidString(rest) {
				t.Fatalf("split produced invalid UTF-8")
			}
		})
	}
}

func TestFenceTracker(t *testing.T) {
	var f fenceTracker
	// Chunks deliberately do not align to lines — that is how they arrive.
	f.consume("intro\n\n``")
	f.consume("`go\nfunc main() {\n")
	if f.reopen() != "```go\n" {
		t.Fatalf("inside a fence, reopen = %q, want %q", f.reopen(), "```go\n")
	}
	if f.closer() != "\n```\n" {
		t.Fatalf("inside a fence, closer = %q, want %q", f.closer(), "\n```\n")
	}
	f.consume("}\n```\ndone\n")
	if f.reopen() != "" {
		t.Fatalf("after the closing delimiter the tracker should be outside the fence, got %q", f.reopen())
	}
}
