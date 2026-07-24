package acp

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

// closeSubscription must retract only the live subscription: when a follow-up
// prompt overwrote `active`, a stale prior turn tearing down must not clear the
// live one.
func TestCloseSubscriptionOnlyRetractsOwn(t *testing.T) {
	c := newACPSession(ProcessOptions{})
	first := &subscription{events: make(chan agent.Event, 1)}
	second := &subscription{events: make(chan agent.Event, 1)}

	// A follow-up prompt reused the session and overwrote the active subscription.
	c.active = second

	// The first (stale) prompt tearing down must NOT clear the live (second) one.
	c.closeSubscription(first)
	if c.active != second {
		t.Fatalf("stale teardown removed the live subscription: got %v", c.active)
	}

	// The live prompt tearing down clears itself.
	c.closeSubscription(second)
	if c.active != nil {
		t.Fatal("live teardown should have cleared active")
	}
}

// A trailing session/update can land on the readLoop at the exact moment a turn
// tears down. deliverNotification captures the subscription under the lock and
// sends on it WITHOUT the lock (the send blocks, deliberately, for back-pressure).
// Teardown must wait for that in-flight send to drain before it closes the
// channel — closing under the send is what panicked the gateway with "send on
// closed channel". This models a sender parked mid-send and asserts
// closeSubscription blocks in its drain barrier until the send completes.
func TestCloseSubscriptionWaitsForInFlightSend(t *testing.T) {
	c := newACPSession(ProcessOptions{})
	sub := &subscription{events: make(chan agent.Event)} // unbuffered: the send blocks until drained
	c.active = sub

	// A readLoop-path send that has already passed the lock (wg.Add under it) and
	// is now parked in the blocking send.
	sub.wg.Add(1)
	sent := make(chan struct{})
	go func() {
		sub.events <- agent.Event{Type: agent.EventText, Text: "trailing"}
		sub.wg.Done()
		close(sent)
	}()

	closed := make(chan struct{})
	go func() {
		c.closeSubscription(sub)
		close(closed)
	}()

	// Teardown must be parked in the drain barrier while the send is in flight.
	select {
	case <-closed:
		t.Fatal("closeSubscription closed the channel under an in-flight send — would panic in prod")
	case <-time.After(100 * time.Millisecond):
	}

	// Drain the send; teardown may now proceed.
	if ev := <-sub.events; ev.Text != "trailing" {
		t.Fatalf("unexpected in-flight event: %+v", ev)
	}
	<-sent
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("closeSubscription did not finish after the in-flight send drained")
	}
	if _, ok := <-sub.events; ok {
		t.Fatal("events channel should be closed after teardown")
	}
	if c.active != nil {
		t.Fatal("subscription should be retracted after teardown")
	}
}

// The real readLoop → deliverNotification path racing teardown, run hot under the
// race detector. With the drain barrier no ordering produces a send on a closed
// channel.
func TestDeliverNotificationDoesNotRaceTeardown(t *testing.T) {
	const chunk = `{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}}`
	for i := 0; i < 200; i++ {
		c := newACPSession(ProcessOptions{})
		sub := &subscription{events: make(chan agent.Event, 32)}
		c.active = sub
		c.scope = promptScope{}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.deliverNotification(rpcNotification{Method: "session/update", Params: json.RawMessage(chunk)})
		}()
		go func() {
			defer wg.Done()
			c.closeSubscription(sub)
		}()
		for range sub.events {
		}
		wg.Wait()
	}
}
