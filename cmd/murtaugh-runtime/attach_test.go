package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/nodesocket"
)

type loopHarness struct {
	t *testing.T

	mu      sync.Mutex
	dialled []string
	waits   []time.Duration

	answer    func(n int, address string) error
	stopAfter int
}

func (h *loopHarness) run() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- attach(ctx, quietLogger(), attachment{
			gateways: []string{seed},
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

// A redirect means the fleet answered, not that it failed; waiting out a backoff
// here would only show up as slow failover in production.
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

// If the hop budget were never refilled, one early misconfiguration would stop
// the node following redirects for the rest of its life.
func TestABackoffReturnsTheHopBudget(t *testing.T) {
	a, b := seed, "wss://b.example.com:8787"
	h := &loopHarness{
		t:         t,
		stopAfter: 2*(maxConsecutiveHops+1) + 1,
		answer: func(_ int, _ string) error {
			return &nodesocket.RedirectError{Addresses: []string{a, b}}
		},
	}
	h.run()

	if got := h.waitCount(); got != 2 {
		t.Errorf("the loop backed off %d times over two redirect chains, want 2: "+
			"the hop budget was not returned after the node paid a wait", got)
	}
}

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
