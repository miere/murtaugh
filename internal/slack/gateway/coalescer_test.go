package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/slack-go/slack"
)

// fakeTimer is a stopper whose callback fires only when the test says so, giving
// the coalescer tests deterministic control over the debounce window.
type fakeTimer struct {
	mu      sync.Mutex
	fn      func()
	stopped bool
	fired   bool
}

func (t *fakeTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	was := !t.stopped && !t.fired
	t.stopped = true
	return was
}

func (t *fakeTimer) fire() bool {
	t.mu.Lock()
	if t.stopped || t.fired {
		t.mu.Unlock()
		return false
	}
	t.fired = true
	fn := t.fn
	t.mu.Unlock()
	fn()
	return true
}

// fakeTimerFactory hands out fakeTimers and lets a test fire the most recently
// armed one (the pending debounce for the batch just submitted).
type fakeTimerFactory struct {
	mu     sync.Mutex
	timers []*fakeTimer
}

func (f *fakeTimerFactory) after(_ time.Duration, fn func()) stopper {
	t := &fakeTimer{fn: fn}
	f.mu.Lock()
	f.timers = append(f.timers, t)
	f.mu.Unlock()
	return t
}

func (f *fakeTimerFactory) fireLast() bool {
	f.mu.Lock()
	var t *fakeTimer
	for i := len(f.timers) - 1; i >= 0; i-- {
		c := f.timers[i]
		c.mu.Lock()
		pending := !c.stopped && !c.fired
		c.mu.Unlock()
		if pending {
			t = c
			break
		}
	}
	f.mu.Unlock()
	if t == nil {
		return false
	}
	return t.fire()
}

// dispatchRecorder captures what the coalescer dispatches.
type dispatchRecorder struct {
	mu   sync.Mutex
	reqs []ChatRequest
}

func (d *dispatchRecorder) dispatch(_ context.Context, _ agent.ConversationKey, _ string, _ ChatRoute, req ChatRequest) {
	d.mu.Lock()
	d.reqs = append(d.reqs, req)
	d.mu.Unlock()
}

func (d *dispatchRecorder) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.reqs)
}

func (d *dispatchRecorder) last() ChatRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reqs[len(d.reqs)-1]
}

func waitUntil(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.After(d)
	for !cond() {
		select {
		case <-deadline:
			t.Fatal("condition not met before deadline")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

var testKey = agent.ConversationKey{ChannelID: "C", ThreadTS: "1"}

func TestCoalescerIdleDispatchesOnlyAfterDebounce(t *testing.T) {
	timers := &fakeTimerFactory{}
	rec := &dispatchRecorder{}
	c := newCoalescer(time.Hour, timers.after,
		func(string) bool { return true },
		func(agent.ConversationKey) bool { return false },
		rec.dispatch, discardLogger())

	c.submit(context.Background(), testKey, "a", ChatRoute{}, ChatRequest{Text: "hi"})
	if rec.count() != 0 {
		t.Fatal("must not dispatch before the debounce fires")
	}
	timers.fireLast()
	if rec.count() != 1 || rec.last().Text != "hi" {
		t.Fatalf("want one dispatch of 'hi', got %d: %+v", rec.count(), rec.reqs)
	}
}

func TestCoalescerBatchesRapidMessagesIntoOneTurn(t *testing.T) {
	timers := &fakeTimerFactory{}
	rec := &dispatchRecorder{}
	c := newCoalescer(time.Hour, timers.after,
		func(string) bool { return true },
		func(agent.ConversationKey) bool { return false },
		rec.dispatch, discardLogger())

	// Three messages before the window elapses arm exactly one debounce timer.
	c.submit(context.Background(), testKey, "a", ChatRoute{}, ChatRequest{Text: "a", Files: []slack.File{{ID: "f1"}}})
	c.submit(context.Background(), testKey, "a", ChatRoute{}, ChatRequest{Text: "b"})
	c.submit(context.Background(), testKey, "a", ChatRoute{}, ChatRequest{Text: "c", Files: []slack.File{{ID: "f2"}}})
	timers.fireLast()

	if rec.count() != 1 {
		t.Fatalf("rapid messages must coalesce into one turn, got %d", rec.count())
	}
	got := rec.last()
	if got.Text != "a\n\nb\n\nc" {
		t.Fatalf("coalesced text = %q, want joined a/b/c", got.Text)
	}
	if len(got.Files) != 2 {
		t.Fatalf("coalesced files = %d, want 2 merged", len(got.Files))
	}
}

func TestCoalescerMidTurnInterruptibleCancelsThenDrains(t *testing.T) {
	timers := &fakeTimerFactory{}
	rec := &dispatchRecorder{}
	var cancels int
	var mu sync.Mutex
	c := newCoalescer(time.Hour, timers.after,
		func(string) bool { return true }, // interruptible
		func(agent.ConversationKey) bool { mu.Lock(); cancels++; mu.Unlock(); return true },
		rec.dispatch, discardLogger())

	c.submit(context.Background(), testKey, "a", ChatRoute{}, ChatRequest{Text: "first"})
	timers.fireLast() // dispatch first; running

	c.submit(context.Background(), testKey, "a", ChatRoute{}, ChatRequest{Text: "second"})
	timers.fireLast() // running + interruptible -> cancel the in-flight turn
	mu.Lock()
	gotCancels := cancels
	mu.Unlock()
	if gotCancels != 1 {
		t.Fatalf("expected one interrupt of the in-flight turn, got %d", gotCancels)
	}
	if rec.count() != 1 {
		t.Fatal("the follow-up must wait for the cancelled turn to complete before dispatch")
	}

	c.onComplete(testKey) // the cancelled turn ends -> drain the coalesced follow-up
	if rec.count() != 2 || rec.last().Text != "second" {
		t.Fatalf("expected the coalesced follow-up to dispatch on completion, got %+v", rec.reqs)
	}
}

func TestCoalescerMidTurnNonInterruptibleWaitsThenDrains(t *testing.T) {
	timers := &fakeTimerFactory{}
	rec := &dispatchRecorder{}
	var cancels int
	var mu sync.Mutex
	c := newCoalescer(time.Hour, timers.after,
		func(string) bool { return false }, // NOT interruptible
		func(agent.ConversationKey) bool { mu.Lock(); cancels++; mu.Unlock(); return true },
		rec.dispatch, discardLogger())

	c.submit(context.Background(), testKey, "a", ChatRoute{}, ChatRequest{Text: "first"})
	timers.fireLast() // dispatch first; running

	c.submit(context.Background(), testKey, "a", ChatRoute{}, ChatRequest{Text: "second"})
	timers.fireLast() // running + non-interruptible -> wait, do NOT cancel or dispatch
	mu.Lock()
	gotCancels := cancels
	mu.Unlock()
	if gotCancels != 0 {
		t.Fatalf("a non-interruptible turn must not be cancelled, got %d cancels", gotCancels)
	}
	if rec.count() != 1 {
		t.Fatal("the follow-up must wait for the running turn to finish naturally")
	}

	c.onComplete(testKey) // running turn finishes -> drain the coalesced follow-up
	if rec.count() != 2 || rec.last().Text != "second" {
		t.Fatalf("expected the follow-up to dispatch on completion, got %+v", rec.reqs)
	}
}

func TestCoalescerClearDropsPending(t *testing.T) {
	timers := &fakeTimerFactory{}
	rec := &dispatchRecorder{}
	c := newCoalescer(time.Hour, timers.after,
		func(string) bool { return false },
		func(agent.ConversationKey) bool { return false },
		rec.dispatch, discardLogger())

	c.submit(context.Background(), testKey, "a", ChatRoute{}, ChatRequest{Text: "first"})
	timers.fireLast() // dispatch first; running
	c.submit(context.Background(), testKey, "a", ChatRoute{}, ChatRequest{Text: "second"})

	c.clear(testKey)      // /stop drops the queued follow-up
	c.onComplete(testKey) // turn ends; nothing pending to drain
	if rec.count() != 1 {
		t.Fatalf("cleared follow-up must not dispatch, got %d", rec.count())
	}
}

func TestCoalesceJoinFormat(t *testing.T) {
	// A lone message is returned unchanged.
	solo := coalesce([]ChatRequest{{Text: "only", MessageTS: "1"}})
	if solo.Text != "only" || solo.MessageTS != "1" {
		t.Fatalf("single message must pass through unchanged, got %+v", solo)
	}
	// Several are joined in order; blanks skipped; files merged; last is the base.
	merged := coalesce([]ChatRequest{
		{Text: "a", Files: []slack.File{{ID: "f1"}}},
		{Text: "   "},
		{Text: "c", MessageTS: "9", Files: []slack.File{{ID: "f2"}}},
	})
	if merged.Text != "a\n\nc" {
		t.Fatalf("joined text = %q, want \"a\\n\\nc\"", merged.Text)
	}
	if merged.MessageTS != "9" {
		t.Fatalf("base must be the last message (MessageTS 9), got %q", merged.MessageTS)
	}
	if len(merged.Files) != 2 {
		t.Fatalf("merged files = %d, want 2", len(merged.Files))
	}
}
