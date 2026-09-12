package mcpbridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/miere/murtaugh/internal/tools"
)

type echoTool struct {
	name    string
	invoked *bool
}

func (e *echoTool) Name() string                    { return e.name }
func (e *echoTool) Description() string             { return "echo tool" }
func (e *echoTool) InputSchema() *jsonschema.Schema { return nil }
func (e *echoTool) Invoke(_ context.Context, _ map[string]any) (any, error) {
	if e.invoked != nil {
		*e.invoked = true
	}
	return "echoed", nil
}

// startServer launches a Server on a short temp socket and waits until it is
// accepting connections. Short path matters: unix socket paths are length-capped
// (~104 bytes on macOS), and t.TempDir() can exceed that.
func startServer(t *testing.T) (*Server, context.Context) {
	t.Helper()
	srv, ctx, _ := startLoggingServer(t)
	return srv, ctx
}

// syncBuffer collects log lines the server writes from its own goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

func startLoggingServer(t *testing.T) (*Server, context.Context, *syncBuffer) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mb")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logs := &syncBuffer{}
	srv := NewServer(socket, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { _ = srv.Close() })
	go func() { _ = srv.Start(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); err == nil {
			return srv, ctx, logs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("server socket never appeared")
	return nil, nil, nil
}

// connectThroughBridge wires an MCP client to the server through a RunBridge
// pipe, returning a live client session.
func connectThroughBridge(t *testing.T, ctx context.Context, socket, token string) *mcpsdk.ClientSession {
	t.Helper()
	clientToBridge, bridgeIn := io.Pipe()    // client writes bridgeIn -> bridge reads clientToBridge
	bridgeOut, clientFromBridge := io.Pipe() // bridge writes clientFromBridge -> client reads bridgeOut

	go func() { _ = RunBridge(ctx, socket, token, clientToBridge, clientFromBridge) }()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "v0"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.IOTransport{Reader: bridgeOut, Writer: bridgeIn}, nil)
	if err != nil {
		t.Fatalf("client connect through bridge: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestBridgeEndToEnd(t *testing.T) {
	srv, ctx := startServer(t)
	invoked := false
	token, err := srv.Register(Session{Tools: []tools.Tool{&echoTool{name: "ping", invoked: &invoked}}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	session := connectThroughBridge(t, ctx, srv.SocketPath(), token)

	list, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != "ping" {
		t.Fatalf("ListTools through bridge = %+v, want one ping tool", list.Tools)
	}

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "ping"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !invoked {
		t.Fatal("tool was not invoked through the bridge")
	}
	if tc, ok := res.Content[0].(*mcpsdk.TextContent); !ok || tc.Text != "echoed" {
		t.Fatalf("CallTool result = %+v, want echoed", res.Content)
	}
}

func TestBridgeRejectsUnknownToken(t *testing.T) {
	srv, ctx := startServer(t)
	// Never register; any token is unknown. The server closes the connection
	// after the handshake, so the client's MCP handshake must fail.
	clientToBridge, bridgeIn := io.Pipe()
	bridgeOut, clientFromBridge := io.Pipe()
	go func() { _ = RunBridge(ctx, srv.SocketPath(), "bogus-token", clientToBridge, clientFromBridge) }()

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "v0"}, nil)
	connectCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := client.Connect(connectCtx, &mcpsdk.IOTransport{Reader: bridgeOut, Writer: bridgeIn}, nil); err == nil {
		t.Fatal("client connected with an unknown token; want failure")
	}
}

func TestUnregisterStopsNewClaims(t *testing.T) {
	srv, _ := startServer(t)
	token, err := srv.Register(Session{Tools: []tools.Tool{&echoTool{name: "ping"}}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	srv.Unregister(token)
	srv.mu.Lock()
	_, ok := srv.sessions[token]
	srv.mu.Unlock()
	if ok {
		t.Fatal("token still registered after Unregister")
	}
}

// Only the session's own backend knows the alias; the same tool keeps its name in
// every other session.
func TestBridgePublishesUnderTheSessionsAliases(t *testing.T) {
	srv, ctx := startServer(t)
	aliased, err := srv.Register(Session{Tools: []tools.Tool{&echoTool{name: "ask"}}, Aliases: map[string]string{"ask": "AskUserQuestion"}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	plain, err := srv.Register(Session{Tools: []tools.Tool{&echoTool{name: "ask"}}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	for token, want := range map[string]string{aliased: "AskUserQuestion", plain: "ask"} {
		list, err := connectThroughBridge(t, ctx, srv.SocketPath(), token).ListTools(ctx, &mcpsdk.ListToolsParams{})
		if err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		if len(list.Tools) != 1 || list.Tools[0].Name != want {
			t.Errorf("session published %+v, want one tool named %s", list.Tools, want)
		}
	}
}

// Every new conversation is tried as a resume first; the process torn down when
// that fails takes its bridge with it, and the warning that produced fired on
// every new conversation.
func TestAnAggregatorSessionWarnsOnlyWhenItEndsUnexpectedly(t *testing.T) {
	logs := &syncBuffer{}
	srv := NewServer("unused", slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	for _, err := range []error{io.EOF, net.ErrClosed, context.Canceled,
		fmt.Errorf("read unix @->/tmp/mb/s: %w", net.ErrClosed)} {
		logs.reset()
		srv.logSessionEnd(err)
		if strings.Contains(logs.String(), "level=WARN") {
			t.Errorf("a bridge that went away with its agent was logged as a warning:\n%s", logs.String())
		}
		if !strings.Contains(logs.String(), "level=DEBUG") {
			t.Errorf("a bridge teardown (%v) left no trace at all:\n%s", err, logs.String())
		}
	}

	logs.reset()
	srv.logSessionEnd(errors.New("jsonrpc: malformed frame"))
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("an aggregator session that ended unexpectedly was not warned about:\n%s", logs.String())
	}
}

// The end-to-end shape of the same thing: a live session whose bridge is killed
// must leave no warning behind.
func TestKillingABridgeLeavesNoWarning(t *testing.T) {
	srv, ctx, logs := startLoggingServer(t)
	token, err := srv.Register(Session{Tools: []tools.Tool{&echoTool{name: "ping"}}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	bridgeCtx, killBridge := context.WithCancel(ctx)
	session := connectThroughBridge(t, bridgeCtx, srv.SocketPath(), token)
	if _, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{}); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	killBridge()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), "mcp aggregator session ended") {
			if strings.Contains(logs.String(), "level=WARN") {
				t.Fatalf("killing a bridge warned:\n%s", logs.String())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the aggregator never reported the session ending:\n%s", logs.String())
}
