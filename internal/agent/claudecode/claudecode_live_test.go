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

// TestLivePermissionAskRoundTrip drives the full permission path against the real
// binary: `--permission-prompt-tool stdio` makes claude ASK, our client raises an
// EventPermission, the test answers "allow", and the Write tool then runs — proving
// enablement + control-protocol answer + tool execution end to end.
func TestLivePermissionAskRoundTrip(t *testing.T) {
	work := t.TempDir()
	c := New(Options{
		Command:          liveBinary(t),
		Args:             liveArgs(),
		WorkDir:          work,
		PermissionPolicy: "ask",
	})
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// A unique thread id so we create a fresh session rather than resume an old one.
	meta := agent.SessionMetadata{TeamID: "TLIVE", ChannelID: "CLIVE", ThreadTS: "perm-" + work}
	sess, err := c.NewSession(ctx, meta)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	ch, err := c.Prompt(ctx, sess.ID, agent.PromptRequest{Text: "Create a file named probe.txt containing the word hi, using the Write tool. Then say done."})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	var asked bool
	deadline := time.After(80 * time.Second)
	for done := false; !done; {
		select {
		case ev, ok := <-ch:
			if !ok {
				done = true
				break
			}
			switch ev.Type {
			case agent.EventPermission:
				asked = true
				t.Logf("permission asked: kind=%q title=%q", ev.Permission.Request.ToolKind, ev.Permission.Request.ToolTitle)
				ev.Permission.Decision <- "allow"
			case agent.EventComplete:
				done = true
			case agent.EventError:
				t.Fatalf("turn errored: %v", ev.Error)
			}
		case <-deadline:
			t.Fatal("live permission turn timed out")
		}
	}

	if !asked {
		t.Fatal("real claude never asked for permission — enablement (--permission-prompt-tool stdio) may have regressed")
	}
	if _, err := os.Stat(work + "/probe.txt"); err != nil {
		t.Fatalf("expected the Write tool to have run after allow, but probe.txt is missing: %v", err)
	}
}
