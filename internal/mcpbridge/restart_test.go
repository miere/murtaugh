package mcpbridge

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/miere/murtaugh/internal/tools"
)

// newServerAt builds a Server on a short temp socket path without starting it.
// Short path matters: unix socket paths are length-capped (~104 bytes on macOS),
// and t.TempDir() can exceed that.
func newServerAt(t *testing.T) *Server {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mb")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	srv := NewServer(filepath.Join(dir, "s"), nil)
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// runUntil starts srv on its own cancellable context and waits for the socket to
// appear. The returned cancel stops that run, the way a demotion cancels the
// gateway's serve context.
func runUntil(t *testing.T, srv *Server) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Start(ctx) }()
	waitForSocket(t, srv.SocketPath(), true)
	return ctx, cancel
}

func waitForSocket(t *testing.T, path string, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, err := os.Stat(path)
		if (err == nil) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if want {
		t.Fatalf("socket %s never appeared", path)
	}
	t.Fatalf("socket %s was never removed", path)
}

// serveTools registers a session and drives one full bridge conversation over
// the socket: dial, handshake, MCP initialise, ListTools. It is the assertion
// that matters, because the failure being guarded against is a server that binds
// a listener and then refuses everything that arrives on it — indistinguishable
// from health if you only check that the socket file exists.
func serveTools(ctx context.Context, srv *Server) error {
	token, err := srv.Register(Session{Tools: []tools.Tool{&echoTool{name: "ping"}}})
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	defer srv.Unregister(token)

	clientToBridge, bridgeIn := io.Pipe()
	bridgeOut, clientFromBridge := io.Pipe()
	go func() {
		_ = RunBridge(ctx, srv.SocketPath(), token, clientToBridge, clientFromBridge)
		// Unblock a client waiting on the read side however the bridge ended: a
		// failed dial returns before anything is ever written to the pipe, and the
		// MCP client would otherwise wait on it forever.
		_ = clientFromBridge.Close()
	}()
	defer func() { _ = bridgeIn.Close() }()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "v0"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.IOTransport{Reader: bridgeOut, Writer: bridgeIn}, nil)
	if err != nil {
		return fmt.Errorf("connect through bridge: %w", err)
	}
	defer func() { _ = session.Close() }()

	list, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{})
	if err != nil {
		return fmt.Errorf("ListTools: %w", err)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != "ping" {
		return fmt.Errorf("ListTools = %+v, want one ping tool", list.Tools)
	}
	return nil
}

// assertServesTools runs serveTools under a wall-clock bound, so a server that
// accepts a connection and then says nothing fails the test instead of hanging it.
func assertServesTools(t *testing.T, srv *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveTools(ctx, srv) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("aggregator did not serve its tools: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("aggregator never answered")
	}
}

// A gateway that is demoted and promoted again calls startBridge a second time
// on a fresh serve context. The aggregator has to come back: an acp or
// claude_code agent reaches every Murtaugh tool through it, its session managers
// survive demotion, and the socket path is stable — so a server that refuses to
// restart leaves those agents tool-less and silent for the rest of the process,
// with one log line as the only evidence.
func TestServerServesAgainAfterItsContextEnds(t *testing.T) {
	srv := newServerAt(t)

	_, cancel1 := runUntil(t, srv)
	assertServesTools(t, srv)

	cancel1()
	waitForSocket(t, srv.SocketPath(), false)

	runUntil(t, srv)
	assertServesTools(t, srv)
}

// The same sequence with no pause between the demotion and the promotion — the
// shape of a fast failover, where StopServing does not wait for the bridge
// goroutine before the next StartServing runs.
//
// It does not, by itself, prove the superseded run cannot tear down its
// successor: the scheduler almost always runs the old watcher before the new
// Start binds. TestASupersededRunDoesNotTearDownItsSuccessor forces the other
// order.
func TestServerSurvivesAnImmediateRestart(t *testing.T) {
	srv := newServerAt(t)

	_, cancel1 := runUntil(t, srv)
	cancel1()

	// Deliberately no wait: the second run starts while the first is still
	// unwinding. runUntil is not used here because it waits for the socket FILE,
	// which the first run has not necessarily removed yet — it would return on
	// the old run's socket and prove nothing.
	ctx2, cancel2 := context.WithCancel(context.Background())
	t.Cleanup(cancel2)
	go func() { _ = srv.Start(ctx2) }()

	// Long enough for the superseded run's watcher to have fired against the new
	// one, and for the new listener to be bound.
	time.Sleep(300 * time.Millisecond)
	assertServesTools(t, srv)
}

// waitForRunAfter blocks until srv has begun a run later than prev.
func waitForRunAfter(t *testing.T, srv *Server, prev uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if run, _ := currentRun(srv); run > prev {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no run after %d ever started", prev)
}

// currentRun reports which run is live and the listener it bound.
func currentRun(srv *Server) (uint64, net.Listener) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.run, srv.listener
}

// A run's context watcher can wake up after a newer run has already bound the
// socket — the goroutine is not tracked by anything the demotion waits on, so
// nothing orders it before the next promotion. When it does, it must take down
// only its own listener.
//
// The interleaving is produced directly rather than raced for, because the
// scheduler almost always runs the watcher first and a test that relies on
// losing that race proves nothing on the runs where it wins.
func TestASupersededRunDoesNotTearDownItsSuccessor(t *testing.T) {
	srv := newServerAt(t)

	runUntil(t, srv)
	staleRun, staleListener := currentRun(srv)

	// A second promotion, superseding the first while its watcher has yet to fire.
	// Its context is never cancelled here: the point is what the FIRST run's
	// watcher does to it.
	ctx2, cancel2 := context.WithCancel(context.Background())
	t.Cleanup(cancel2)
	go func() { _ = srv.Start(ctx2) }()
	waitForRunAfter(t, srv, staleRun)

	// Now the first run's watcher gets its turn.
	_ = srv.stop(staleRun, staleListener)

	assertServesTools(t, srv)
}

// gatedListener holds a Close open until the test releases it, so a test can put
// another goroutine's work strictly inside a stop's teardown. It is possible
// only because stop is handed the listener it must close rather than reading the
// server's own.
type gatedListener struct {
	net.Listener
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (l *gatedListener) Close() error {
	l.once.Do(func() { close(l.entered) })
	<-l.release
	return l.Listener.Close()
}

// The narrow case the run counter alone does not cover: a superseded run decides
// it is still current, and only THEN — while it is closing its listener — the
// next promotion binds the socket. Deciding once at the top and unlinking
// afterwards deletes the successor's socket file, and every agent that has not
// already connected gets ENOENT on a server that believes it is serving.
//
// The interleaving is forced rather than raced for: the stale run is parked
// inside its listener's Close while the new run binds.
func TestASupersededRunDoesNotUnlinkTheSocketItRacedWith(t *testing.T) {
	srv := newServerAt(t)

	runUntil(t, srv)
	staleRun, staleListener := currentRun(srv)

	gated := &gatedListener{
		Listener: staleListener,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = srv.stop(staleRun, gated)
	}()

	// stop has passed its "am I current?" test and is inside the listener Close.
	select {
	case <-gated.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("stop never reached the listener close")
	}

	// The promotion lands here, in the middle of the stale run's teardown.
	ctx2, cancel2 := context.WithCancel(context.Background())
	t.Cleanup(cancel2)
	go func() { _ = srv.Start(ctx2) }()
	waitForRunAfter(t, srv, staleRun)

	close(gated.release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop never returned")
	}

	assertServesTools(t, srv)
}
