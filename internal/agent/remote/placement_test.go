package remote

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
)

// This is the test for the placement decision, which is the load-bearing choice
// in this package.
//
// Six optional capability surfaces are type-asserted for on this path. Two are
// asserted on the CLIENT (internal/agent/session_manager.go:44 and :53) and are
// answered by this package. The other four are asserted on the MANAGER, from
// the gateway: Warm (chat_handler.go:413), an anonymous
// Discard(agent.ConversationKey) (chat_handler.go:404), an anonymous
// Interruptible() bool (gateway.go:1983) and io.Closer (gateway.go:861).
//
// Because the remote client goes UNDER *agent.SessionManager rather than in
// place of it, those four keep being answered by the manager, unchanged. The
// assertions below are copied from those call sites verbatim: if a later change
// moves the remote client up to ChatSessionManager, this test is what says what
// it has to re-satisfy.
//
// Three of the four fail SILENTLY when unsatisfied — a manager that does not
// implement Discard disarms the idle-timeout drop, the tool-ceiling drop and
// the credential-rejection drop at once, with no log line — so "it compiles"
// proves nothing here.
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
	// The verdict came off the wire, through the client surface the manager
	// probes: proof that folding it into the initialize answer keeps the
	// manager's own reporting honest.
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

	// The surface that could NOT have been degraded: on two of the three
	// backends a session owns a real process, so a no-op here leaks one per
	// evicted conversation.
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
