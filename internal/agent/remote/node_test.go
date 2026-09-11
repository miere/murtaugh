package remote

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/nodelink"
)

type fakeNode struct {
	t    *testing.T
	link *nodelink.Link

	interruptible *bool
	sessionID     string
	rejectPrompt  error
	eagerEvent    *agentwire.Event

	mu       sync.Mutex
	metadata agentwire.SessionMetadata

	prompts   chan agentwire.PromptBody
	streams   chan string
	cancels   chan string
	released  chan string
	goodbyes  chan struct{}
	decisions chan agentwire.PermissionResponse
	responses chan agentwire.Message
	answers   chan agentwire.DisplayAnswer
}

type nodeConfig struct {
	interruptible *bool
	sessionID     string
	rejectPrompt  error
	noSessionID   bool
	eagerEvent    *agentwire.Event
	dropFrame     func(payload []byte) bool
}

func newFakeNode(t *testing.T, conn nodelink.Conn, cfg nodeConfig) *fakeNode {
	t.Helper()
	n := &fakeNode{
		t:             t,
		interruptible: cfg.interruptible,
		sessionID:     cfg.sessionID,
		rejectPrompt:  cfg.rejectPrompt,
		eagerEvent:    cfg.eagerEvent,
		prompts:       make(chan agentwire.PromptBody, 8),
		streams:       make(chan string, 8),
		cancels:       make(chan string, 8),
		released:      make(chan string, 8),
		goodbyes:      make(chan struct{}, 8),
		decisions:     make(chan agentwire.PermissionResponse, 8),
		responses:     make(chan agentwire.Message, 8),
		answers:       make(chan agentwire.DisplayAnswer, 8),
	}
	if n.sessionID == "" && !cfg.noSessionID {
		n.sessionID = "session-42"
	}
	if cfg.dropFrame != nil {
		conn = &lossyConn{Conn: conn, drop: cfg.dropFrame}
	}
	n.link = nodelink.New(conn, nodelink.Options{Handler: n.consume, Logger: discardLogger()})
	return n
}

func (n *fakeNode) consume(payload []byte) error {
	msg, err := agentwire.DecodeMessage(payload)
	if err != nil {
		return err
	}
	switch msg.Kind {
	case agentwire.MessageRequest:
		n.serve(msg)
	case agentwire.MessagePermission:
		var answer agentwire.PermissionResponse
		if err := msg.Into(&answer); err != nil {
			return err
		}
		n.decisions <- answer
	case agentwire.MessageResponse:
		n.responses <- msg
	case agentwire.MessageAnswer:
		var answer agentwire.DisplayAnswer
		if err := msg.Into(&answer); err != nil {
			return err
		}
		n.answers <- answer
	}
	return nil
}

func (n *fakeNode) serve(msg agentwire.Message) {
	switch msg.Method {
	case agentwire.MethodInitialize:
		n.reply(msg.ID, agentwire.InitializeResult{Interruptible: n.interruptible})
	case agentwire.MethodNewSession:
		var meta agentwire.SessionMetadata
		if err := msg.Into(&meta); err != nil {
			n.t.Errorf("node: read metadata: %v", err)
			return
		}
		n.mu.Lock()
		n.metadata = meta
		n.mu.Unlock()
		n.reply(msg.ID, agentwire.NewSessionResult{SessionID: n.sessionID})
	case agentwire.MethodPrompt:
		var body agentwire.PromptBody
		if err := msg.Into(&body); err != nil {
			n.t.Errorf("node: read prompt: %v", err)
			return
		}
		n.prompts <- body
		if n.rejectPrompt != nil {
			n.send(agentwire.Fault(msg.ID, n.rejectPrompt))
			return
		}
		if n.eagerEvent != nil {
			n.emit(msg.ID, *n.eagerEvent)
		}
		n.reply(msg.ID, agentwire.Empty{})
		n.streams <- msg.ID
	case agentwire.MethodCancel:
		var ref agentwire.SessionRef
		if err := msg.Into(&ref); err != nil {
			n.t.Errorf("node: read session ref: %v", err)
			return
		}
		n.cancels <- ref.SessionID
		n.reply(msg.ID, agentwire.Empty{})
	case agentwire.MethodCloseSession:
		var ref agentwire.SessionRef
		if err := msg.Into(&ref); err != nil {
			n.t.Errorf("node: read session ref: %v", err)
			return
		}
		n.released <- ref.SessionID
	case agentwire.MethodClose:
		n.goodbyes <- struct{}{}
		n.reply(msg.ID, agentwire.Empty{})
	}
}

func (n *fakeNode) reply(id string, body any) {
	msg, err := agentwire.Result(id, body)
	if err != nil {
		n.t.Errorf("node: build result: %v", err)
		return
	}
	n.send(msg)
}

func (n *fakeNode) send(msg agentwire.Message) {
	raw, err := msg.Encode()
	if err != nil {
		n.t.Errorf("node: encode %s: %v", msg.Kind, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := n.link.Send(ctx, raw); err != nil {
		n.t.Logf("node: send %s: %v", msg.Kind, err)
	}
}

func (n *fakeNode) emit(streamID string, ev agentwire.Event) {
	msg, err := agentwire.StreamEvent(streamID, ev)
	if err != nil {
		n.t.Errorf("node: build event: %v", err)
		return
	}
	n.send(msg)
}

func (n *fakeNode) end(streamID string) { n.send(agentwire.StreamEnd(streamID)) }

func (n *fakeNode) emitBackground(sessionID string, ev agentwire.Event) {
	msg, err := agentwire.BackgroundEvent(sessionID, ev)
	if err != nil {
		n.t.Errorf("node: build background event: %v", err)
		return
	}
	n.send(msg)
}

func (n *fakeNode) seenMetadata() agentwire.SessionMetadata {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.metadata
}

type lossyConn struct {
	nodelink.Conn
	drop func(payload []byte) bool
}

func (c *lossyConn) WriteMessage(raw []byte) error {
	env, err := nodelink.Decode(raw)
	if err == nil && env.Kind == nodelink.KindMessage && c.drop(env.Payload) {
		return nil
	}
	return c.Conn.WriteMessage(raw)
}

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&logBuffer{}, &slog.HandlerOptions{Level: slog.LevelError}))
}

func dial(t *testing.T, cfg nodeConfig, opts Options) (*Client, *fakeNode, *logBuffer) {
	t.Helper()
	gatewaySide, nodeSide := nodelink.Pipe(32)
	node := newFakeNode(t, nodeSide, cfg)
	logs := &logBuffer{}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	client := New(gatewaySide, opts)
	t.Cleanup(func() {
		_ = client.Close()
		_ = node.link.Close()
	})
	return client, node, logs
}

func receive[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		var zero T
		t.Fatalf("timed out waiting for %s", what)
		return zero
	}
}
