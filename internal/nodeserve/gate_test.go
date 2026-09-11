package nodeserve

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/nodelink"
)

// Delegated runs, jobs and unfurls have no approver in process either, so the gate must not invent
// a refusal for them.
func TestAnUngatedCallOutsideATurnIsAllowed(t *testing.T) {
	gate := NewToolGate(nil)

	allowed, note := gate.Approve(context.Background(), "terminal", "ls")

	if !allowed {
		t.Fatalf("a call outside a turn was refused with %q; delegated runs are ungated in process and must stay so", note)
	}
	if note != "" {
		t.Fatalf("an allowed call carried a note: %q", note)
	}
}

func TestATurnWithNoConnectionIsDeniedWithAReason(t *testing.T) {
	gate := NewToolGate(nil)

	allowed, note := gate.Approve(withStream(context.Background(), "turn-1"), "terminal", "rm -rf /")

	if allowed {
		t.Fatal("a side-effecting call was allowed while no gateway was attached")
	}
	if note == "" {
		t.Fatal("a refused call carried no note, so the model is told nothing about why it did not run")
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// The thread is the gateway's to know; a thread id in that field would be read as a session that
// does not exist.
func TestAnApprovalNamesTheNodesSession(t *testing.T) {
	gate := NewToolGate(discardLogger())
	frames := make(chan agentwire.Message, 16)
	gatewayConn, nodeConn := nodelink.Pipe(8)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = Serve(ctx, nodeConn, &approvingClient{gate: gate}, Options{Logger: discardLogger(), Gate: gate})
	}()
	link := nodelink.New(gatewayConn, nodelink.Options{
		Handler: func(raw []byte) error {
			if msg, err := agentwire.DecodeMessage(raw); err == nil {
				frames <- msg
			}
			return nil
		},
		Logger: discardLogger(),
	})
	t.Cleanup(func() {
		cancel()
		_ = link.Close()
		<-stopped
	})

	(&pipePeer{link: link}).request(t, "p1", agentwire.MethodPrompt, agentwire.PromptBody{
		SessionID: "session-7",
		Prompt:    agentwire.EncodePromptRequest(agent.PromptRequest{Text: "clean up", Channel: "C1", Thread: "123.4"}),
	})
	for {
		select {
		case msg := <-frames:
			var ev agentwire.Event
			if msg.Kind != agentwire.MessageEvent || msg.Into(&ev) != nil || ev.Permission == nil {
				continue
			}
			if ev.Permission.SessionID != "session-7" {
				t.Fatalf("the approval names session %q, want the node's session-7", ev.Permission.SessionID)
			}
			return
		case <-time.After(10 * time.Second):
			t.Fatal("the node never raised its approval")
		}
	}
}

type approvingClient struct {
	gate *ToolGate
}

func (c *approvingClient) Initialize(context.Context) error { return nil }

func (c *approvingClient) NewSession(context.Context, agent.SessionMetadata) (agent.Session, error) {
	return agent.Session{ID: "session-7"}, nil
}

func (c *approvingClient) Prompt(ctx context.Context, _ string, req agent.PromptRequest) (<-chan agent.Event, error) {
	events := make(chan agent.Event)
	go func() {
		defer close(events)
		turnCtx := agent.WithTurnLocation(ctx, agent.TurnLocation{ChannelID: req.Channel, ThreadTS: req.Thread})
		c.gate.Approve(turnCtx, "terminal", "rm -rf /tmp/x")
	}()
	return events, nil
}

func (c *approvingClient) Cancel(context.Context, string) error { return nil }
func (c *approvingClient) Close() error                         { return nil }
