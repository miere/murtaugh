package nodehost

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agent/remote"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/nodelink"
)

// The two registry properties that cannot be reached from outside the package:
// what happens when several goroutines close one entry at the same instant, and
// what the session map holds after a connection leaves.
//
// Everything else in this package is tested over the real loopback rig. These
// two are here because the state they are about — a `chan struct{}` and an
// unexported map — has no observable projection: the first fails as a panic on
// a goroutine no test owns, and the second is a leak that never changes an
// answer.

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// noTokens is a credential store that answers nothing. These tests never
// authenticate anything; New refuses a nil store, which is the only reason it
// is here.
type noTokens struct{}

func (noTokens) Put(context.Context, config.NodeToken) error { return nil }
func (noTokens) BySelector(context.Context, string) (config.NodeToken, bool, error) {
	return config.NodeToken{}, false, nil
}
func (noTokens) List(context.Context, string) ([]config.NodeToken, error) { return nil, nil }
func (noTokens) Revoke(context.Context, string, time.Time) (config.NodeToken, bool, error) {
	return config.NodeToken{}, false, nil
}
func (noTokens) Close() error { return nil }

func testHost(t *testing.T) *Host {
	t.Helper()
	host, err := New(Options{Tokens: noTokens{}, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	return host
}

// newEntry is a registry entry over a pipe, which is what makes close() real:
// it closes a live remote.Client rather than a nil one.
//
// The node's end of the pipe is closed first, so the client's goodbye fails on
// the transport instead of waiting out its five-second answer timeout against a
// peer that will never answer. What is under test is the entry's own
// bookkeeping, not the close handshake, which nodelink tests.
func newEntry(t *testing.T, connID string) *attached {
	t.Helper()
	gatewaySide, nodeSide := nodelink.Pipe(8)
	_ = nodeSide.Close()
	node := &attached{connID: connID, nodeID: "node-1", closed: make(chan struct{})}
	node.client = remote.New(gatewaySide, remote.Options{Logger: quietLogger()})
	return node
}

// Four production paths close one entry, and any two of them can arrive
// together: the link's own serve loop on detach, shutdown's detachAll,
// revocation's CloseCredential, and a redial displacing this connection.
//
// A check-then-close on the channel is `panic: close of closed channel` — not an
// error anybody handles, but a panic on a goroutine with no recover above it,
// which takes the gateway daemon down. It happens on shutdown and on a
// credential revocation: the two moments least likely to be watched, and the two
// where the daemon dying looks like the thing that was asked for.
//
// The barrier is what makes this deterministic rather than a race one run in
// twenty loses, and it is a SPIN rather than a channel on purpose: releasing
// eight goroutines by closing a channel wakes them one at a time through the
// scheduler, which is enough serialisation to hide the window entirely — a
// channel barrier over these same rounds reproduced nothing. Each closer
// reports its own panic instead of crashing the suite, so a regression fails
// this test rather than every test in the package.
func TestClosingOneConnectionFromSeveralGoroutinesAtOnceIsSafe(t *testing.T) {
	const closers = 8
	const rounds = 2000

	var mu sync.Mutex
	var panics []any

	for round := 0; round < rounds; round++ {
		node := newEntry(t, "1")
		var start atomic.Bool
		var wg sync.WaitGroup
		for i := 0; i < closers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() {
					if r := recover(); r != nil {
						mu.Lock()
						panics = append(panics, r)
						mu.Unlock()
					}
				}()
				for !start.Load() {
				}
				node.close()
			}()
		}
		start.Store(true)
		wg.Wait()

		select {
		case <-node.closed:
		default:
			t.Fatal("the entry was not closed at all")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(panics) > 0 {
		t.Fatalf("closing one connection concurrently panicked %d times (%v); a shutdown or a revocation would take the gateway daemon with it",
			len(panics), panics[0])
	}
}

// A departing connection takes its session bindings with it.
//
// sessionNode would answer correctly without this — it re-checks the registry
// before trusting an entry — so nothing observable changes and that is exactly
// why it needs its own test: the cost of losing it is a map that grows by one
// entry for every session of every node that ever attached, on a gateway that
// runs for months in front of laptops that connect and disconnect all day.
func TestASessionBindingLeavesWithItsConnection(t *testing.T) {
	host := testHost(t)
	node := newEntry(t, "1")

	host.mu.Lock()
	host.nodes[node.connID] = node
	host.mu.Unlock()
	host.bindSession("session-a", node)
	host.bindSession("session-b", node)

	host.remove(node)

	host.mu.Lock()
	held := len(host.sessions)
	host.mu.Unlock()
	if held != 0 {
		t.Fatalf("the registry still holds %d session bindings for a connection that has gone", held)
	}
	if _, err := host.sessionNode("session-a"); !errors.Is(err, agent.ErrSessionGone) {
		t.Fatalf("a session on a departed connection resolved with %v, want ErrSessionGone", err)
	}
}

// Revocation and shutdown prune too. They take a different path out of the
// registry — takeCredential and takeAll, neither of which goes through remove —
// so each has to do it itself.
func TestRevocationAndShutdownPruneTheirSessionBindings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		leave func(*Host, *attached)
	}{
		{"revocation", func(h *Host, n *attached) { h.takeCredential(n.selector) }},
		{"shutdown", func(h *Host, n *attached) { h.takeAll() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := testHost(t)
			node := newEntry(t, "1")
			node.selector = "sel-1"

			host.mu.Lock()
			host.nodes[node.connID] = node
			host.mu.Unlock()
			host.bindSession("session-a", node)

			tc.leave(host, node)

			host.mu.Lock()
			held := len(host.sessions)
			host.mu.Unlock()
			if held != 0 {
				t.Fatalf("%s left %d session bindings behind", tc.name, held)
			}
		})
	}
}
