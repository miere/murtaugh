package acp

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

// TestConcurrentConversationsGetSeparateProcesses is the isolation smoke test for
// the per-conversation-process design: two conversations on one manager must run
// in two DISTINCT agent processes (no shared-process taint) and answer
// independently, even when the agent hands back the SAME session id from each
// process (the fake helper always returns "session-1") — the manager routes by
// its own key, so the two must not collide onto one process.
func TestConcurrentConversationsGetSeparateProcesses(t *testing.T) {
	helper := []string{"-test.run", "TestACPHelperProcess", "--", "acp-helper"}
	c := NewProcessClient(ProcessOptions{Command: os.Args[0], Args: helper})
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
	if a.ID == b.ID {
		t.Fatalf("two conversations collided on manager key %q", a.ID)
	}

	// The two conversations must be backed by two DISTINCT processes.
	c.mu.Lock()
	sa, sb := c.sessions[a.ID], c.sessions[b.ID]
	c.mu.Unlock()
	if sa == nil || sb == nil {
		t.Fatalf("a session was not registered: a=%v b=%v", sa, sb)
	}
	if sa.cmd.Process.Pid == sb.cmd.Process.Pid {
		t.Fatalf("both conversations share process %d — not isolated", sa.cmd.Process.Pid)
	}

	// Each answers independently.
	for _, s := range []agent.Session{a, b} {
		ch, err := c.Prompt(ctx, s.ID, agent.PromptRequest{Text: "hi"})
		if err != nil {
			t.Fatalf("Prompt %s: %v", s.ID, err)
		}
		gotText := false
		for ev := range ch {
			switch ev.Type {
			case agent.EventError:
				t.Fatalf("session %s errored: %v", s.ID, ev.Error)
			case agent.EventText:
				gotText = true
			}
		}
		if !gotText {
			t.Fatalf("session %s produced no reply", s.ID)
		}
	}

	// Eviction of one conversation tears down only its process.
	c.CloseSession(a.ID)
	c.mu.Lock()
	_, aStill := c.sessions[a.ID]
	_, bStill := c.sessions[b.ID]
	c.mu.Unlock()
	if aStill {
		t.Fatal("CloseSession did not drop the conversation")
	}
	if !bStill {
		t.Fatal("CloseSession dropped the wrong conversation")
	}
}
