//go:build claudelive

// This file is a LIVE integration test that drives the real `claude` binary. It
// is excluded from normal builds and CI by the `claudelive` build tag, so it
// never runs (or even compiles) in a standard `go test ./...`. It is kept — not
// thrown away — because it is the only check that exercises the actual Claude
// Code stream-json + control protocol end-to-end, which is invaluable when the
// protocol drifts across `claude` versions.
//
// Run it deliberately:
//
//	go test -tags claudelive ./internal/agent/claudecode/ -run TestLive -v
//
// It needs a working `claude` on PATH (or CLAUDE_BIN) with valid auth (OAuth
// subscription or ANTHROPIC_API_KEY) and network access. It uses a cheap model
// and a no-tool prompt, so it neither spends much nor needs tool permissions.
package claudecode

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

func liveBinary(t *testing.T) string {
	t.Helper()
	if bin := strings.TrimSpace(os.Getenv("CLAUDE_BIN")); bin != "" {
		return bin
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude binary not found on PATH and CLAUDE_BIN unset; skipping live test")
	}
	return bin
}

func liveArgs() []string {
	args := append([]string{}, defaultArgs...)
	return append(args, "--model", "claude-haiku-4-5-20251001")
}

// TestLiveHandshakeAndTurn drives the real binary through the initialize
// handshake and one no-tool turn, asserting the reply streams back and the turn
// completes. This is the residual Phase 1 checkpoint from spec 019 §6.
func TestLiveHandshakeAndTurn(t *testing.T) {
	c := New(Options{Command: liveBinary(t), Args: liveArgs()})
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize (handshake) against real claude: %v", err)
	}
	sess, err := c.NewSession(ctx, agent.SessionMetadata{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Logf("live session id: %q", sess.ID)

	ch, err := c.Prompt(ctx, sess.ID, agent.PromptRequest{Text: "Reply with exactly: pong"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var text string
	var completed bool
	deadline := time.After(75 * time.Second)
	for !completed {
		select {
		case ev, ok := <-ch:
			if !ok {
				completed = true
				break
			}
			switch ev.Type {
			case agent.EventText:
				text += ev.Text
			case agent.EventComplete:
				t.Logf("completed: stop_reason=%q", ev.StopReason)
				completed = true
			case agent.EventError:
				t.Fatalf("turn errored: %v", ev.Error)
			}
		case <-deadline:
			t.Fatalf("live turn timed out; text so far: %q", text)
		}
	}

	if !strings.Contains(strings.ToLower(text), "pong") {
		t.Fatalf("expected reply to contain 'pong', got %q", text)
	}
}
