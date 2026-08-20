package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/slack-go/slack"
)

// recordingStreamAPI wraps the real Slack client, remembering every message it
// opens so the manual end-to-end test can delete them afterwards.
type recordingStreamAPI struct {
	*slack.Client
	channel string
	opened  []string
}

func (r *recordingStreamAPI) StartStreamContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error) {
	ch, ts, err := r.Client.StartStreamContext(ctx, channelID, options...)
	if err == nil {
		r.opened = append(r.opened, ts)
	}
	return ch, ts, err
}

// TestStreamWriterAgainstRealSlack is the end-to-end check that the size budget
// matches the live API. It is skipped unless MURTAUGH_E2E_CHANNEL is set, because
// it posts to a real workspace — the unit tests above cover the same behaviour
// against the fake and are what CI runs.
//
//	SLACK_BOT_TOKEN=… MURTAUGH_E2E_CHANNEL=C… MURTAUGH_E2E_TEAM=T… \
//	  go test ./internal/slack/gateway/ -run TestStreamWriterAgainstRealSlack -v
//
// It cleans up after itself: every message it opens is deleted before it returns.
func TestStreamWriterAgainstRealSlack(t *testing.T) {
	channel := os.Getenv("MURTAUGH_E2E_CHANNEL")
	token := os.Getenv("SLACK_BOT_TOKEN")
	if channel == "" || token == "" {
		t.Skip("set SLACK_BOT_TOKEN and MURTAUGH_E2E_CHANNEL to run the live check")
	}
	api := &recordingStreamAPI{Client: slack.New(token), channel: channel}
	ctx := context.Background()
	// Stream into a thread off a parent we own, so the probe stays out of the
	// channel scroll and takes its parent with it on the way out.
	_, parent, err := api.Client.PostMessageContext(ctx, channel,
		slack.MsgOptionText("stream size check — self-deleting", false))
	if err != nil {
		t.Fatalf("post parent: %v", err)
	}
	t.Cleanup(func() {
		for _, ts := range append([]string{parent}, api.opened...) {
			if _, _, err := api.Client.DeleteMessageContext(ctx, channel, ts); err != nil {
				t.Logf("cleanup: could not delete %s: %v", ts, err)
			}
		}
		t.Logf("cleanup: deleted %d message(s)", len(api.opened)+1)
	})

	// A reply shaped like the one that vanished: long prose, a code block that
	// straddles the boundary, and em-dashes so the rune/byte distinction is live.
	var reply strings.Builder
	for i := 0; i < 120; i++ {
		fmt.Fprintf(&reply, "Paragraph %d — prose long enough to matter, with an em-dash and some filler text.\n\n", i)
	}
	reply.WriteString("```go\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&reply, "\tfmt.Println(%q) // line %d\n", "a line of code", i)
	}
	reply.WriteString("```\n")
	body := reply.String()
	t.Logf("reply is %d chars / %d bytes", utf8.RuneCountInString(body), len(body))

	writer := NewStreamWriter(api, channel, StreamWriterOptions{
		Interval: time.Hour,
		MinChars: 1,
		ThreadTS: parent,
		TeamID:   os.Getenv("MURTAUGH_E2E_TEAM"),
		UserID:   os.Getenv("MURTAUGH_E2E_USER"),
		Logger:   slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
	if err := writer.Append(ctx, body); err != nil {
		t.Fatalf("Append against real Slack: %v", err)
	}
	if err := writer.Stop(ctx); err != nil {
		t.Fatalf("Stop against real Slack: %v", err)
	}
	if len(api.opened) < 2 {
		t.Fatalf("expected the reply to span several streaming messages, got %d", len(api.opened))
	}
	t.Logf("delivered across %d streaming messages", len(api.opened))
}
