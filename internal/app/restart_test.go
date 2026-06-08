package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestRestartCoordinator_FirstRequestCancelsAndExits is the happy path:
// one accepted Request must cancel the root context exactly once and call
// the configured exit hook with code 0 after the grace window.
func TestRestartCoordinator_FirstRequestCancelsAndExits(t *testing.T) {
	var cancels int32
	cancel := context.CancelFunc(func() { atomic.AddInt32(&cancels, 1) })
	exited := make(chan int, 1)

	c := NewRestartCoordinator(cancel, discardLogger(), 100*time.Millisecond, 5*time.Millisecond)
	c.exit = func(code int) { exited <- code }

	if !c.Request(RestartRequest{Source: RestartSourceSlash, UserID: "U1", Reason: "user asked"}) {
		t.Fatal("expected first restart request to be accepted")
	}
	if got := atomic.LoadInt32(&cancels); got != 1 {
		t.Fatalf("cancel call count = %d, want 1", got)
	}
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	case <-time.After(time.Second):
		t.Fatal("exit was not invoked after grace period")
	}
}

// TestRestartCoordinator_SubsequentRequestIgnoredWhileFiring guards the
// dedup invariant: once the watchdog goroutine is running, every other
// caller must be told no, regardless of the request's source.
func TestRestartCoordinator_SubsequentRequestIgnoredWhileFiring(t *testing.T) {
	c := NewRestartCoordinator(func() {}, discardLogger(), time.Second, 200*time.Millisecond)
	c.exit = func(int) {}

	if !c.Request(RestartRequest{Source: RestartSourceSlash, UserID: "U1"}) {
		t.Fatal("first request rejected unexpectedly")
	}
	if c.Request(RestartRequest{Source: RestartSourceInteractive, UserID: "U2"}) {
		t.Fatal("expected second request to be deduplicated while firing")
	}
}

// TestRestartCoordinator_NilCancelIsTolerated covers the wiring path
// where the coordinator is constructed without a real cancel (e.g. when
// the ctx is owned by an outer caller and only the exit watchdog matters).
func TestRestartCoordinator_NilCancelIsTolerated(t *testing.T) {
	c := NewRestartCoordinator(nil, discardLogger(), time.Second, 5*time.Millisecond)
	exited := make(chan int, 1)
	c.exit = func(code int) { exited <- code }

	if !c.Request(RestartRequest{Source: RestartSourceSlash}) {
		t.Fatal("expected nil-cancel request to be accepted")
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("expected exit to fire even without a cancel func")
	}
}

// TestRestartCoordinator_DefaultsFillInForNonPositiveDurations asserts the
// safety nets: zero or negative cooldown/grace must not produce a hot
// loop or an instant exit.
func TestRestartCoordinator_DefaultsFillInForNonPositiveDurations(t *testing.T) {
	c := NewRestartCoordinator(nil, discardLogger(), 0, -1)
	if c.cooldown != defaultRestartCooldown {
		t.Errorf("cooldown = %v, want default %v", c.cooldown, defaultRestartCooldown)
	}
	if c.grace != defaultRestartGrace {
		t.Errorf("grace = %v, want default %v", c.grace, defaultRestartGrace)
	}
}
