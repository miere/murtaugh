package nodehost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agent/remote"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/nodelink"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

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

func newEntry(t *testing.T, connID string) *attached {
	t.Helper()
	gatewaySide, nodeSide := nodelink.Pipe(8)
	_ = nodeSide.Close()
	node := &attached{connID: connID, nodeID: "node-1", closed: make(chan struct{})}
	node.client = remote.New(gatewaySide, remote.Options{Logger: quietLogger()})
	return node
}

// The barrier spins on purpose: a channel barrier serialises the goroutines
// enough to hide the double-close panic.
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

// Nothing observable breaks without the pruning, so only this test stops the
// binding map growing for as long as the gateway runs.
func TestASessionBindingLeavesWithItsConnection(t *testing.T) {
	host := testHost(t)
	node := newEntry(t, "1")

	host.mu.Lock()
	host.nodes[node.connID] = node
	host.mu.Unlock()
	host.bindSession("session-a", node)
	host.bindSession("session-b", node)
	host.markTakeover("session-a", "node-old")

	host.remove(node)

	host.mu.Lock()
	held := len(host.sessions)
	host.mu.Unlock()
	if held != 0 {
		t.Fatalf("the registry still holds %d session bindings for a connection that has gone", held)
	}
	if _, marked := host.takeTakeover("session-a"); marked {
		t.Fatal("a takeover mark outlived the session it belonged to")
	}
	if _, err := host.sessionNode("session-a"); !errors.Is(err, agent.ErrSessionGone) {
		t.Fatalf("a session on a departed connection resolved with %v, want ErrSessionGone", err)
	}
}

// takeCredential and takeAll bypass remove, so each must prune on its own.
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

func TestADepartingConnectionForgetsWhatItReportedAndAConnectionReportsOnFewCredentials(t *testing.T) {
	host := newTestHost(t, nil, config.AccessConfig{})
	node := attachStubs(host, nodeStub{id: "node-a", owner: "U1"})["node-a"]
	for i := range maxCredentialsPerNode + 2 {
		host.reportCredential(node, agentwire.CredentialHealth{Credential: fmt.Sprint("/claude-", i)})
	}
	if got := len(host.credentialReports()); got != maxCredentialsPerNode {
		t.Fatalf("the gateway holds %d reports from one node, want at most %d", got, maxCredentialsPerNode)
	}
	host.remove(node)
	host.mu.Lock()
	defer host.mu.Unlock()
	if node.credentials != nil {
		t.Fatalf("a departed connection still holds %d reports", len(node.credentials))
	}
}
