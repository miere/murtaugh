package agentbuild

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agent/remote"
	"github.com/miere/murtaugh/internal/mcpbridge"
	"github.com/miere/murtaugh/internal/nodelink"
	"github.com/miere/murtaugh/internal/nodeserve"
	"github.com/miere/murtaugh/internal/tools"
	"github.com/miere/murtaugh/internal/tools/attach"
	"github.com/miere/murtaugh/internal/tools/files"
	"github.com/miere/murtaugh/internal/tools/plan"
)

// Claude Code and ACP agents call attach through the bridge; before the emitter,
// the file reached only the model as JSON and never the user.
func TestAttachThroughTheBridgeReachesTheTurn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("# findings"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := files.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	turn := make(chan agent.Event, 1)
	emit := func(ev agent.Event) bool { turn <- ev; return true }

	srv, ctx := startBridge(t)
	token, err := srv.Register(mcpbridge.Session{
		Tools:       []tools.Tool{attach.New(root)},
		WithContext: turnDecorator(agent.SessionMetadata{ChannelID: "C1", ThreadTS: "1.2"}, nil, emit),
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	session := dialBridge(t, ctx, srv.SocketPath(), token)

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "attach", Arguments: map[string]any{"path": "report.md"}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("attach failed: %+v", res.Content)
	}
	select {
	case ev := <-turn:
		if ev.Type != agent.EventAttachment || ev.Attachment == nil || ev.Attachment.Path != filepath.Join(dir, "report.md") {
			t.Fatalf("turn received %+v, want the report as an attachment", ev)
		}
	default:
		t.Fatal("nothing reached the turn")
	}
	text := res.Content[0].(*mcpsdk.TextContent).Text
	if !strings.HasPrefix(text, "Attached report.md") || strings.Contains(text, dir) {
		t.Fatalf("model was told %q, want a confirmation without the host path", text)
	}
}

func startBridge(t *testing.T) (*mcpbridge.Server, context.Context) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := mcpbridge.NewServer(socket, nil)
	t.Cleanup(func() { _ = srv.Close() })
	go func() { _ = srv.Start(ctx) }()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if _, err := os.Stat(socket); err == nil {
			return srv, ctx
		}
	}
	t.Fatal("bridge socket never appeared")
	return nil, nil
}

func dialBridge(t *testing.T, ctx context.Context, socket, token string) *mcpsdk.ClientSession {
	t.Helper()
	clientToBridge, bridgeIn := io.Pipe()
	bridgeOut, clientFromBridge := io.Pipe()
	go func() { _ = mcpbridge.RunBridge(ctx, socket, token, clientToBridge, clientFromBridge) }()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "v0"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.IOTransport{Reader: bridgeOut, Writer: bridgeIn}, nil)
	if err != nil {
		t.Fatalf("connect through bridge: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// An acp or claude_code tool on a node reaches the gateway only through the
// context the aggregator gives it, so the plan goes over the real bridge here.
func TestAPlanThroughTheBridgeIsAnsweredByTheGateway(t *testing.T) {
	srv, ctx := startBridge(t)
	reg := tools.NewRegistry()
	reg.Register(plan.New(agent.TurnDisplay{}))
	aggr, err := newACPAggregator(srv, reg, resolvedFor(t, "", "present_plan"), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("newACPAggregator: %v", err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	gatewayConn, nodeConn := nodelink.Pipe(8)
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = nodeserve.Serve(ctx, nodeConn, &bridgedAgent{ctx: ctx, aggr: aggr, socket: srv.SocketPath()}, nodeserve.Options{Logger: quiet})
	}()
	gateway := remote.New(gatewayConn, remote.Options{Logger: quiet})
	t.Cleanup(func() {
		_ = gateway.Close()
		<-served
	})
	if err := gateway.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	session, err := gateway.NewSession(ctx, agent.SessionMetadata{ChannelID: "C1", ThreadTS: "1.2"})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	events, err := gateway.Prompt(ctx, session.ID, agent.PromptRequest{Text: "migrate", Channel: "C1", Thread: "1.2"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}

	var reply string
	for ev := range events {
		switch ev.Type {
		case agent.EventPlan:
			ev.Plan.Answer <- agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Choice: agent.PlanProceed}
		case agent.EventText:
			reply += ev.Text
		case agent.EventError:
			t.Fatalf("the turn failed: %v", ev.Error)
		}
	}
	if !strings.Contains(reply, "Approved") {
		t.Fatalf("the model was handed %q", reply)
	}
}

type bridgedAgent struct {
	ctx    context.Context
	aggr   *acpAggregator
	socket string

	mu   sync.Mutex
	meta agent.SessionMetadata
}

func (a *bridgedAgent) Initialize(context.Context) error { return nil }

func (a *bridgedAgent) NewSession(_ context.Context, meta agent.SessionMetadata) (agent.Session, error) {
	a.mu.Lock()
	a.meta = meta
	a.mu.Unlock()
	return agent.Session{ID: "bridged-1"}, nil
}

func (a *bridgedAgent) Prompt(context.Context, string, agent.PromptRequest) (<-chan agent.Event, error) {
	events := make(chan agent.Event, 8)
	a.mu.Lock()
	meta := a.meta
	a.mu.Unlock()
	spec, release, err := a.aggr.RegisterSession(meta, func(ev agent.Event) bool { events <- ev; return true })
	if err != nil {
		return nil, err
	}
	go func() {
		defer close(events)
		defer release()
		text, err := a.callPlan(spec.Env[mcpbridge.EnvToken])
		if err != nil {
			events <- agent.Event{Type: agent.EventError, Error: err}
			return
		}
		events <- agent.Event{Type: agent.EventText, Text: text}
	}()
	return events, nil
}

func (a *bridgedAgent) callPlan(token string) (string, error) {
	clientToBridge, bridgeIn := io.Pipe()
	bridgeOut, clientFromBridge := io.Pipe()
	go func() { _ = mcpbridge.RunBridge(a.ctx, a.socket, token, clientToBridge, clientFromBridge) }()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "v0"}, nil)
	session, err := client.Connect(a.ctx, &mcpsdk.IOTransport{Reader: bridgeOut, Writer: bridgeIn}, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = session.Close() }()
	res, err := session.CallTool(a.ctx, &mcpsdk.CallToolParams{Name: "present_plan", Arguments: map[string]any{"plan": "1. back up\n2. migrate"}})
	if err != nil {
		return "", err
	}
	text := res.Content[0].(*mcpsdk.TextContent).Text
	if res.IsError {
		return "", fmt.Errorf("present_plan failed: %s", text)
	}
	return text, nil
}

func (a *bridgedAgent) Cancel(context.Context, string) error { return nil }
func (a *bridgedAgent) Close() error                         { return nil }
