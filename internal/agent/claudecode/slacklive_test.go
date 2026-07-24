//go:build claudelive

// LIVE end-to-end validation of the background auto-continue path against REAL
// Claude Code AND real Slack: it drives a claude_code turn that launches a
// BACKGROUND subagent, and asserts the subagent's post-`result` completion
// reaches OnBackground (the thing only fake-tested elsewhere) and is delivered
// into a Slack thread. Self-skips unless SLACK_BOT_TOKEN + SLACK_TEST_CHANNEL are
// set. The spawn guard strips CLAUDECODE, so it runs from inside a Claude Code
// session with no manual env fiddling.
//
//	SLACK_BOT_TOKEN=xoxb-… SLACK_TEST_CHANNEL=C… \
//	  go test -tags claudelive ./internal/agent/acp/../claudecode/ -run TestLiveBackground -v
package claudecode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

func slackCall(t *testing.T, token, method string, form url.Values) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("POST", "https://slack.com/api/"+method, strings.NewReader(form.Encode()))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("slack %s: %v", method, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("slack %s failed: %v", method, out["error"])
	}
	return out
}

func TestLiveBackgroundSubagentPostsToSlack(t *testing.T) {
	token := strings.TrimSpace(os.Getenv("SLACK_BOT_TOKEN"))
	channel := strings.TrimSpace(os.Getenv("SLACK_TEST_CHANNEL"))
	if token == "" || channel == "" {
		t.Skip("set SLACK_BOT_TOKEN and SLACK_TEST_CHANNEL to run the Slack background live test")
	}

	// Root the test in its own thread.
	root := slackCall(t, token, "chat.postMessage", url.Values{
		"channel": {channel},
		"text":    {":robot_face: claude_code background auto-continue live test — launching a background subagent…"},
	})
	threadTS, _ := root["ts"].(string)

	work := t.TempDir()
	_ = os.WriteFile(work+"/a.txt", []byte("x"), 0o644)
	_ = os.WriteFile(work+"/b.txt", []byte("y"), 0o644)

	// OnBackground stands in for the gateway sink: accumulate the auto-continue
	// text and, on EventComplete, post it into the thread — proving the background
	// completion both reached us AND is deliverable.
	var mu sync.Mutex
	var bg strings.Builder
	posted := make(chan string, 1)
	onBackground := func(sessionID string, ev agent.Event) {
		switch ev.Type {
		case agent.EventText:
			mu.Lock()
			bg.WriteString(ev.Text)
			mu.Unlock()
		case agent.EventComplete:
			mu.Lock()
			text := strings.TrimSpace(bg.String())
			bg.Reset()
			mu.Unlock()
			if text == "" {
				return
			}
			slackCall(t, token, "chat.postMessage", url.Values{
				"channel":   {channel},
				"thread_ts": {threadTS},
				"text":      {"↩️ *background completion:* " + text},
			})
			select {
			case posted <- text:
			default:
			}
		}
	}

	c := New(Options{
		Command:          liveBinary(t),
		Args:             liveArgs(),
		WorkDir:          work,
		PermissionPolicy: "auto-allow", // no human on this path; let the subagent's read tools run
		OnBackground:     onBackground,
	})
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sess, err := c.NewSession(ctx, agent.SessionMetadata{TeamID: "TLIVE", ChannelID: channel, ThreadTS: threadTS})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	ch, err := c.Prompt(ctx, sess.ID, agent.PromptRequest{
		Text: "Launch a subagent IN THE BACKGROUND (use the Agent/Task tool with run_in_background set true) to count the .txt files in the current directory. Do NOT wait for it — immediately reply that you started it, then end your turn.",
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	// Drain the foreground turn to completion (the "I started it" reply).
	var fg strings.Builder
	for ev := range ch {
		if ev.Type == agent.EventText {
			fg.WriteString(ev.Text)
		}
		if ev.Type == agent.EventError {
			t.Fatalf("foreground turn errored: %v", ev.Error)
		}
	}
	t.Logf("foreground reply: %q", strings.TrimSpace(fg.String()))
	slackCall(t, token, "chat.postMessage", url.Values{
		"channel": {channel}, "thread_ts": {threadTS},
		"text": {"foreground reply: " + strings.TrimSpace(fg.String())},
	})

	// The subagent runs after the turn returned. Its completion must reach
	// OnBackground and post into the thread.
	select {
	case text := <-posted:
		t.Logf("BACKGROUND completion reached OnBackground and posted to Slack: %q", text)
	case <-time.After(150 * time.Second):
		t.Fatal("background subagent completion never reached OnBackground")
	}
}
