package claudecode

import (
	"log/slog"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
)

func TestEmitTurnEvent(t *testing.T) {
	s := newProcSession("s1", Options{Logger: slog.Default()})
	if s.emitTurnEvent(agent.Event{Type: agent.EventAttachment}) {
		t.Fatal("reported delivery with no turn in flight")
	}

	sub := &subscription{events: make(chan agent.Event, 1)}
	s.active = sub
	if !s.emitTurnEvent(agent.Event{Type: agent.EventAttachment}) {
		t.Fatal("did not deliver to the turn in flight")
	}
	if ev := <-sub.events; ev.Type != agent.EventAttachment {
		t.Fatalf("turn received %q, want an attachment", ev.Type)
	}

	close(sub.events)
	if s.emitTurnEvent(agent.Event{Type: agent.EventAttachment}) {
		t.Fatal("reported delivery to a turn that had already ended")
	}
}
