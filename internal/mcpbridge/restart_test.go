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

// A demoted gateway's agents keep their sessions and socket path, so a server
// that cannot restart leaves them without Murtaugh's tools for good.
func TestServerServesAgainAfterItsContextEnds(t *testing.T) {
	srv := newServerAt(t)

	_, cancel1 := runUntil(t, srv)
	assertServesTools(t, srv)

	cancel1()
	waitForSocket(t, srv.SocketPath(), false)

	runUntil(t, srv)
	assertServesTools(t, srv)
}

// The scheduler almost always runs the old watcher first, so this rarely hits
// the bad ordering; the superseded-run tests below force it.
func TestServerSurvivesAnImmediateRestart(t *testing.T) {
	srv := newServerAt(t)

	_, cancel1 := runUntil(t, srv)
	cancel1()

	ctx2, cancel2 := context.WithCancel(context.Background())
	t.Cleanup(cancel2)
	go func() { _ = srv.Start(ctx2) }()

	time.Sleep(300 * time.Millisecond)
	assertServesTools(t, srv)
}

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

func currentRun(srv *Server) (uint64, net.Listener) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.run, srv.listener
}

// The ordering is forced rather than raced for, because the scheduler almost
// always runs the old watcher first and a lost race would prove nothing.
func TestASupersededRunDoesNotTearDownItsSuccessor(t *testing.T) {
	srv := newServerAt(t)

	runUntil(t, srv)
	staleRun, staleListener := currentRun(srv)

	ctx2, cancel2 := context.WithCancel(context.Background())
	t.Cleanup(cancel2)
	go func() { _ = srv.Start(ctx2) }()
	waitForRunAfter(t, srv, staleRun)

	_ = srv.stop(staleRun, staleListener)

	assertServesTools(t, srv)
}

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

// Checking "still current" once and unlinking later would delete the successor's
// socket, so the stale run is parked inside Close while the new run binds.
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

	select {
	case <-gated.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("stop never reached the listener close")
	}

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
