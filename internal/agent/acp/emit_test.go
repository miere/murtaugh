package acp

import (
	"context"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
)

func TestEmitTurnEvent(t *testing.T) {
	c := newACPSession(ProcessOptions{})
	if c.emitTurnEvent(agent.Event{Type: agent.EventAttachment}) {
		t.Fatal("reported delivery with no turn in flight")
	}

	sub := &subscription{events: make(chan agent.Event, 1)}
	c.active = sub
	if !c.emitTurnEvent(agent.Event{Type: agent.EventAttachment}) {
		t.Fatal("did not deliver to the turn in flight")
	}
	if ev := <-sub.events; ev.Type != agent.EventAttachment {
		t.Fatalf("turn received %q, want an attachment", ev.Type)
	}
	sub.wg.Wait()
}

// Teardown waits on the drain barrier, so an emit into an ending turn must give up
// rather than block forever on a channel nobody will read.
func TestEmitTurnEventGivesUpWhenTheTurnEnds(t *testing.T) {
	c := newACPSession(ProcessOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.active = &subscription{events: make(chan agent.Event)}
	c.scope = promptScope{ctx: ctx}
	if c.emitTurnEvent(agent.Event{Type: agent.EventAttachment}) {
		t.Fatal("reported delivery to a turn that had already ended")
	}
	c.active.wg.Wait()
}
