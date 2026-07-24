package acp

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

func TestProcessClientUnsubscribeOnlyRetractsOwnSubscription(t *testing.T) {
	c := NewProcessClient(ProcessOptions{Command: "true"})
	first := &subscription{events: make(chan agent.Event, 1)}
	second := &subscription{events: make(chan agent.Event, 1)}

	c.subscribers["s"] = first
	// A second prompt reuses the session and overwrites the subscriber.
	c.subscribers["s"] = second

	// The first prompt tearing down must NOT remove the live (second)
	// subscription.
	c.unsubscribe("s", first)
	if c.subscribers["s"] != second {
		t.Fatalf("stale teardown removed the live subscription: got %v", c.subscribers["s"])
	}

	// The live prompt tearing down removes itself.
	c.unsubscribe("s", second)
	if _, ok := c.subscribers["s"]; ok {
		t.Fatal("live teardown should have removed the subscription")
	}
}

// A trailing session/update can land on the readLoop at the exact moment a turn
// tears down. deliverNotification captures the subscription under the lock and
// then sends on it WITHOUT the lock (the send blocks, deliberately, for
// back-pressure). Teardown must wait for that in-flight send to drain before it
// closes the channel — closing under the send is what panicked the gateway with
// "send on closed channel" (process_client.go readLoop → deliverNotification).
// This models a sender parked mid-send and asserts closeSubscription blocks in
// its drain barrier until the send completes, then closes.
func TestCloseSubscriptionWaitsForInFlightSend(t *testing.T) {
	c := NewProcessClient(ProcessOptions{Command: "true"})
	sub := &subscription{events: make(chan agent.Event)} // unbuffered: the send blocks until drained
	c.subscribers["s"] = sub

	// A readLoop-path send that has already passed the lock (wg.Add under it) and
	// is now parked in the blocking send — exactly the state deliverNotification is
	// in when a late notification arrives during teardown.
	sub.wg.Add(1)
	sent := make(chan struct{})
	go func() {
		sub.events <- agent.Event{Type: agent.EventText, Text: "trailing"}
		sub.wg.Done()
		close(sent)
	}()

	closed := make(chan struct{})
	go func() {
		c.closeSubscription("s", sub)
		close(closed)
	}()

	// Teardown must be parked in the drain barrier while the send is in flight; it
	// must NOT have closed the channel yet.
	select {
	case <-closed:
		t.Fatal("closeSubscription closed the channel under an in-flight send — would panic in prod")
	case <-time.After(100 * time.Millisecond):
	}

	// Drain the send; the sender completes and teardown may now proceed.
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
	if _, ok := c.subscribers["s"]; ok {
		t.Fatal("subscription should be retracted after teardown")
	}
}

// The real readLoop → deliverNotification path racing teardown, run hot under the
// race detector. On the pre-fix code this panics ("send on closed channel") once
// the close lands inside deliverNotification's send window; with the drain
// barrier no ordering produces a send on a closed channel.
func TestDeliverNotificationDoesNotRaceTeardown(t *testing.T) {
	const chunk = `{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"x"}}}`
	for i := 0; i < 200; i++ {
		c := NewProcessClient(ProcessOptions{Command: "true"})
		sub := &subscription{events: make(chan agent.Event, 32)}
		c.subscribers["s"] = sub
		c.dests["s"] = promptScope{}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.deliverNotification(rpcNotification{Method: "session/update", Params: json.RawMessage(chunk)})
		}()
		go func() {
			defer wg.Done()
			c.closeSubscription("s", sub)
		}()
		// Teardown closes the channel, so this range terminates; it also drains any
		// event the sender enqueued before the close so nothing blocks.
		for range sub.events {
		}
		wg.Wait()
	}
}
