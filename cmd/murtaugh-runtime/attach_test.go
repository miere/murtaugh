package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/nodesocket"
)

// gateways_test.go covers gatewayList. This file covers the LOOP around it,
// because two of the behaviours #197 headlines are the loop's and not the
// list's:
//
//   - a redirect naming the leader is followed at once, with no backoff wait
//     between the refusal and the hop;
//   - a backoff that has been PAID returns the hop budget, so a node that met a
//     redirect loop on its first morning still follows redirects for the rest
//     of its life.
//
// Both were deletable with `go test ./cmd/murtaugh-runtime/...` green, because
// TestARedirectLoopStopsSpinning calls gateways.waited() itself rather than
// driving the loop, which reaches past the caller under test.

// loopHarness drives attach() with no socket and no clock: every dial is
// answered from a script, and every backoff wait is recorded and returned
// immediately.
type loopHarness struct {
	t *testing.T

	mu      sync.Mutex
	dialled []string
	waits   []time.Duration

	// answer decides what the nth dial returns.
	answer func(n int, address string) error
	// stopAfter cancels the loop once this many dials have been made, which is
	// the only way out of a loop whose whole job is never to give up.
	stopAfter int
}

func (h *loopHarness) run() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- attach(ctx, quietLogger(), attachment{
			gateway: seed,
			dial: func(_ context.Context, address string) (*nodesocket.Conn, error) {
				h.mu.Lock()
				h.dialled = append(h.dialled, address)
				n := len(h.dialled)
				h.mu.Unlock()
				if n >= h.stopAfter {
					cancel()
				}
				return nil, h.answer(n, address)
			},
			wait: func(d time.Duration) <-chan time.Time {
				h.mu.Lock()
				h.waits = append(h.waits, d)
				h.mu.Unlock()
				fired := make(chan time.Time, 1)
				fired <- time.Now()
				return fired
			},
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			h.t.Fatalf("attach: %v", err)
		}
	case <-time.After(5 * time.Second):
		h.t.Fatal("attach did not return after its context was cancelled")
	}
}

func (h *loopHarness) waitCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.waits)
}

func (h *loopHarness) addresses() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.dialled...)
}

// TestTheLoopHopsWithoutWaitingOutABackoff pins "the node hops at once".
//
// A redirect is not a failure — the fleet answered and named the leader — so
// inheriting a backoff earned by unrelated failures would leave a healthy node
// idle for up to half a minute holding the address it needs. Lose this and
// "hops at once" silently becomes "hops after up to 30s", visible only as slow
// failover in production.
func TestTheLoopHopsWithoutWaitingOutABackoff(t *testing.T) {
	leader := "wss://leader.example.com:8787"
	h := &loopHarness{
		t:         t,
		stopAfter: 2,
		answer: func(_ int, address string) error {
			return &nodesocket.RedirectError{Endpoint: address, Addresses: []string{leader}}
		},
	}
	h.run()

	if got := h.waitCount(); got != 0 {
		t.Errorf("the node waited %d times before following a redirect, want 0", got)
	}
	dialled := h.addresses()
	if len(dialled) < 2 || dialled[1] != leader {
		t.Fatalf("after a redirect the node dialled %v, want the leader %q second", dialled, leader)
	}
}

// TestABackoffReturnsTheHopBudget pins "the budget is per chain, not per
// process".
//
// With the budget spent once and never returned, a node that met a
// misconfiguration on its first morning refuses to follow a redirect for the
// rest of the process's life — and finds every later failover only by working
// through its address list one backoff at a time. That is invisible until the
// failover that needed it.
func TestABackoffReturnsTheHopBudget(t *testing.T) {
	a, b := seed, "wss://b.example.com:8787"
	h := &loopHarness{
		t: t,
		// Two full chains: the budget is spent, a backoff pays for it, and the
		// budget must come back for the second chain to hop at all.
		stopAfter: 2*(maxConsecutiveHops+1) + 1,
		answer: func(_ int, _ string) error {
			return &nodesocket.RedirectError{Addresses: []string{a, b}}
		},
	}
	h.run()

	// One wait per exhausted chain, and no more. Without the budget returning,
	// every dial after the first exhaustion is a wait.
	if got := h.waitCount(); got != 2 {
		t.Errorf("the loop backed off %d times over two redirect chains, want 2: "+
			"the hop budget was not returned after the node paid a wait", got)
	}
}

// TestTheLoopKeepsRedialling is the ordinary case the two above are carved out
// of: a gateway that is simply down costs one backoff per attempt, and the node
// never gives up.
func TestTheLoopKeepsRedialling(t *testing.T) {
	h := &loopHarness{
		t:         t,
		stopAfter: 4,
		answer:    func(int, string) error { return errNoAnswer() },
	}
	h.run()

	if got := h.waitCount(); got < 3 {
		t.Errorf("the loop waited %d times over 4 failed dials, want one per dial", got)
	}
}
