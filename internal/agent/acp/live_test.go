//go:build acplive

// LIVE integration test that drives the real Claude Code ACP adapter
// (claude-code-acp / claude-agent-acp). Excluded from normal builds and CI by the
// `acplive` build tag. It is the real-agent smoke test the fake-helper suite can't
// be: it proves two concurrent conversations run in separate processes AND don't
// share context — the isolation the per-conversation-process design exists for.
//
// Run it deliberately (needs the adapter on PATH or CLAUDE_ACP_BIN, plus Claude
// auth + network):
//
//	go test -tags acplive ./internal/agent/acp/ -run TestLive -v
//
// Runs even from inside a Claude Code session: the backend strips the nested-CC
// marker before spawning the adapter (see agent.SpawnEnv), so no manual `env -u
// CLAUDECODE` is needed.
package acp

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

func acpLiveBinary(t *testing.T) string {
	t.Helper()
	if bin := strings.TrimSpace(os.Getenv("CLAUDE_ACP_BIN")); bin != "" {
		return bin
	}
	for _, name := range []string{"claude-agent-acp", "claude-code-acp"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("no ACP adapter on PATH (claude-agent-acp/claude-code-acp) and CLAUDE_ACP_BIN unset")
	return ""
}

// liveReply sends one prompt to a conversation and returns the concatenated reply
// text once the turn completes.
func liveReply(t *testing.T, ctx context.Context, c *Client, id, prompt string) string {
	t.Helper()
	ch, err := c.Prompt(ctx, id, agent.PromptRequest{Text: prompt})
	if err != nil {
		t.Fatalf("Prompt %s: %v", id, err)
	}
	var text string
	deadline := time.After(90 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return text
			}
			switch ev.Type {
			case agent.EventText:
				text += ev.Text
			case agent.EventError:
				t.Fatalf("session %s errored: %v", id, ev.Error)
			case agent.EventComplete:
				// keep draining until the channel closes
			}
		case <-deadline:
			t.Fatalf("session %s timed out; text so far %q", id, text)
		}
	}
}

// TestLiveTwoConversationsIsolated is the real-agent isolation smoke test.
func TestLiveTwoConversationsIsolated(t *testing.T) {
	c := NewProcessClient(ProcessOptions{
		Command:          acpLiveBinary(t),
		WorkDir:          t.TempDir(),
		Env:              []string{"ANTHROPIC_MODEL=claude-haiku-4-5-20251001"},
		PermissionPolicy: "auto-deny", // no tools needed; never block on a human
	})
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	a, err := c.NewSession(ctx, agent.SessionMetadata{ThreadTS: "1"})
	if err != nil {
		t.Fatalf("NewSession a: %v", err)
	}
	b, err := c.NewSession(ctx, agent.SessionMetadata{ThreadTS: "2"})
	if err != nil {
		t.Fatalf("NewSession b: %v", err)
	}

	// Distinct processes — the core isolation guarantee. Also log the agent-side
	// session ids: if the real adapter numbers them per-process (both "1"), that is
	// exactly the collision the manager-owned routing key guards against.
	c.mu.Lock()
	sa, sb := c.sessions[a.ID], c.sessions[b.ID]
	c.mu.Unlock()
	t.Logf("A: key=%s agentSessionID=%q pid=%d", a.ID, sa.sessionID, sa.cmd.Process.Pid)
	t.Logf("B: key=%s agentSessionID=%q pid=%d", b.ID, sb.sessionID, sb.cmd.Process.Pid)
	if sa.cmd.Process.Pid == sb.cmd.Process.Pid {
		t.Fatalf("both conversations share process %d — not isolated", sa.cmd.Process.Pid)
	}

	// Plant a fact in conversation A, then ask B — B must not know it.
	_ = liveReply(t, ctx, c, a.ID, "Remember this number: 42. Reply with just: ok")
	rb := liveReply(t, ctx, c, b.ID, "What number were you asked to remember earlier in this conversation? Reply with only the number, or the word none.")
	if strings.Contains(rb, "42") {
		t.Fatalf("conversation B leaked conversation A's context (saw 42): %q", rb)
	}
	t.Logf("isolation confirmed: B did not see A's planted fact (B replied %q)", strings.TrimSpace(rb))
}
