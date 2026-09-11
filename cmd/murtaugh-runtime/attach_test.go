package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/nodesocket"
	"github.com/miere/murtaugh/internal/nodetoken"
)

type loopHarness struct {
	t *testing.T

	mu        sync.Mutex
	dialled   []string
	presented []string
	waits     []time.Duration

	tokenFile string
	onWait    func()
	answer    func(n int, address string) error
	stopAfter int
}

func (h *loopHarness) run() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if h.tokenFile == "" {
		h.tokenFile = writeToken(h.t, filepath.Join(h.t.TempDir(), nodetoken.FileName), "mrtg_node_0123456789abcdef_first")
	}
	done := make(chan error, 1)
	go func() {
		done <- attach(ctx, quietLogger(), attachment{
			gateways:  []string{seed},
			tokenFile: h.tokenFile,
			dial: func(_ context.Context, address, token string) (*nodesocket.Conn, error) {
				h.mu.Lock()
				h.dialled = append(h.dialled, address)
				h.presented = append(h.presented, token)
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
				if h.onWait != nil {
					h.onWait()
				}
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

func writeToken(t *testing.T, path, token string) string {
	t.Helper()
	if err := nodetoken.WriteFile(path, token); err != nil {
		t.Fatalf("write the node token: %v", err)
	}
	return path
}

// Without this, an operator who replaced a rejected credential would still
// have to restart the node before the new one was ever presented.
func TestEachDialPresentsTheCredentialOnDiskNow(t *testing.T) {
	path := writeToken(t, filepath.Join(t.TempDir(), nodetoken.FileName), "mrtg_node_0123456789abcdef_old")
	h := &loopHarness{t: t, tokenFile: path, stopAfter: 2}
	h.answer = func(n int, _ string) error {
		if n == 1 {
			if err := os.Remove(path); err != nil {
				t.Errorf("remove the old token: %v", err)
			}
			writeToken(t, path, "mrtg_node_0123456789abcdef_new")
		}
		return nodesocket.ErrCredentialRejected
	}
	h.run()

	presented := h.tokensPresented()
	want := []string{"mrtg_node_0123456789abcdef_old", "mrtg_node_0123456789abcdef_new"}
	if len(presented) < 2 || !slices.Equal(presented[:2], want) {
		t.Errorf("the node presented %v over two dials, want %v", presented, want)
	}
}

// The mode check is the only guard on a file that is the node's whole identity,
// so re-reading it must not become a way around it.
func TestADialIsSkippedWhileTheCredentialIsReadableByOthers(t *testing.T) {
	path := writeToken(t, filepath.Join(t.TempDir(), nodetoken.FileName), "mrtg_node_0123456789abcdef_old")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("loosen the token's mode: %v", err)
	}
	h := &loopHarness{t: t, tokenFile: path, stopAfter: 1}
	h.answer = func(int, string) error { return errNoAnswer() }
	var once sync.Once
	dialsBeforeFix := -1
	h.onWait = func() {
		once.Do(func() {
			h.mu.Lock()
			dialsBeforeFix = len(h.dialled)
			h.mu.Unlock()
			if err := os.Chmod(path, nodetoken.FileMode); err != nil {
				t.Errorf("restore the token's mode: %v", err)
			}
		})
	}
	h.run()

	if dialsBeforeFix != 0 {
		t.Errorf("the node dialled %d times with a credential others could read, want 0", dialsBeforeFix)
	}
	if presented := h.tokensPresented(); len(presented) == 0 || presented[0] != "mrtg_node_0123456789abcdef_old" {
		t.Errorf("the node presented %v, want the token once its mode was fixed", presented)
	}
}

func (h *loopHarness) tokensPresented() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.presented)
}
