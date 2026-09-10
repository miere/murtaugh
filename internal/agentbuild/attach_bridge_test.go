package agentbuild

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/mcpbridge"
	"github.com/miere/murtaugh/internal/tools"
	"github.com/miere/murtaugh/internal/tools/attach"
	"github.com/miere/murtaugh/internal/tools/files"
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
