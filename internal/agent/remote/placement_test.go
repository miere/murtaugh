package remote

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
)

// Three of these checks fail silently when unmet (a manager without Discard disarms the
// idle, tool-ceiling and credential drops at once), so compiling proves nothing.
func TestSessionManagerKeepsTheGatewaysCapabilitySurfaces(t *testing.T) {
	no := false
	client, node, _ := dial(t, nodeConfig{interruptible: &no}, Options{})
	manager := agent.NewSessionManager(client, time.Minute, 10).WithLogger(discardLogger())

	if _, ok := any(manager).(interface{ Warm(context.Context) error }); !ok {
		t.Fatal("the manager no longer warms; the gateway would skip this agent with no log")
	}
	if _, ok := any(manager).(interface{ Discard(agent.ConversationKey) }); !ok {
		t.Fatal("the manager no longer discards; three recovery paths become silent no-ops")
	}
	if _, ok := any(manager).(interface{ Interruptible() bool }); !ok {
		t.Fatal("the manager no longer reports interruptibility; the gateway would assume true")
	}
	if _, ok := any(manager).(io.Closer); !ok {
		t.Fatal("the manager is no longer an io.Closer; a config reload would leak the node connection")
	}

	ctx := context.Background()
	if err := manager.Warm(ctx); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if manager.Interruptible() {
		t.Fatal("the node said its agent cannot be interrupted and the manager disagreed")
	}

	key := agent.ConversationKey{TeamID: "T1", ChannelID: "C1", ThreadTS: "1700000000.000100"}
	events, err := manager.Prompt(ctx, key, agent.SessionMetadata{TeamID: "T1", ChannelID: "C1"}, agent.PromptRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("prompt through the manager: %v", err)
	}
	stream := receive(t, "the accepted stream", node.streams)
	node.emit(stream, agentwire.Event{Type: agentwire.EventComplete, StopReason: "end_turn"})
	node.end(stream)
	for range events {
	}

	if sid, ok := manager.Lookup(key); !ok || sid != "session-42" {
		t.Fatalf("manager cached session %q (found=%v)", sid, ok)
	}

	manager.Discard(key)
	if released := receive(t, "the node to release the discarded session", node.released); released != "session-42" {
		t.Fatalf("node released %q", released)
	}
	if _, ok := manager.Lookup(key); ok {
		t.Fatal("the manager kept a discarded session")
	}

	if err := manager.Close(); err != nil {
		t.Fatalf("close through the manager: %v", err)
	}
	receive(t, "the node to be told the client is closing", node.goodbyes)
}
