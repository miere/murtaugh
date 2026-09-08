package nodeserve

import (
	"context"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/nodelink"
)

// A request is dispatched to its own goroutine, and this is what that buys.
//
// nodelink acknowledges a frame only once the handler RETURNS, so a request
// served inline holds the read loop for as long as it runs. servePrompt calls
// client.Prompt synchronously and serveInitialize can spawn a process — either
// would then block delivery of the very Cancel frame meant to stop it, and the
// only symptom a user sees is a conversation that will not interrupt.
//
// The mirror claim on the gateway side is guarded by nodehost's loopback tests
// deadlocking without it. This side had nothing, which is the asymmetry that
// makes it easy to lose.
func TestACancelReachesANodeThatIsBusyStartingATurn(t *testing.T) {
	client := &blockingClient{
		entered:   make(chan struct{}, 1),
		release:   make(chan struct{}),
		cancelled: make(chan string, 1),
	}
	peer := serveOverPipe(t, client)

	peer.request(t, "p1", agentwire.MethodPrompt, agentwire.PromptBody{
		SessionID: "session-1",
		Prompt:    agentwire.EncodePromptRequest(agent.PromptRequest{Text: "a long job"}),
	})
	select {
	case <-client.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the node never began the turn")
	}

	// The turn is still inside Prompt. This is the frame that has to get
	// through anyway.
	peer.request(t, "c1", agentwire.MethodCancel, agentwire.SessionRef{SessionID: "session-1"})

	select {
	case sessionID := <-client.cancelled:
		if sessionID != "session-1" {
			t.Fatalf("the backend was asked to cancel %q", sessionID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a cancel could not reach a node busy starting a turn; a user's stop would never arrive")
	}
	close(client.release)
}

// ---- harness ---------------------------------------------------------------

// pipePeer is the gateway's end of a link, raw: it sends frames and does not
// care what comes back. What is under test is which goroutine the node serves
// them on, not what it answers.
type pipePeer struct{ link *nodelink.Link }

func (p *pipePeer) request(t *testing.T, id string, method agentwire.Method, body any) {
	t.Helper()
	msg, err := agentwire.Request(id, method, body)
	if err != nil {
		t.Fatalf("build %s: %v", method, err)
	}
	raw, err := msg.Encode()
	if err != nil {
		t.Fatalf("encode %s: %v", method, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.link.Send(ctx, raw); err != nil {
		t.Fatalf("send %s: %v", method, err)
	}
}

func serveOverPipe(t *testing.T, client agent.Client) *pipePeer {
	t.Helper()
	gatewayConn, nodeConn := nodelink.Pipe(8)
	ctx, cancel := context.WithCancel(context.Background())

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = Serve(ctx, nodeConn, client, Options{Logger: discardLogger()})
	}()

	link := nodelink.New(gatewayConn, nodelink.Options{
		Handler: func([]byte) error { return nil },
		Logger:  discardLogger(),
	})
	t.Cleanup(func() {
		cancel()
		_ = link.Close()
		<-stopped
	})
	return &pipePeer{link: link}
}

// blockingClient parks inside Prompt until it is released, which is what a
// backend spawning a process or waiting on a model looks like from here.
type blockingClient struct {
	entered   chan struct{}
	release   chan struct{}
	cancelled chan string
}

func (c *blockingClient) Initialize(context.Context) error { return nil }

func (c *blockingClient) NewSession(context.Context, agent.SessionMetadata) (agent.Session, error) {
	return agent.Session{ID: "session-1"}, nil
}

func (c *blockingClient) Prompt(ctx context.Context, _ string, _ agent.PromptRequest) (<-chan agent.Event, error) {
	select {
	case c.entered <- struct{}{}:
	default:
	}
	select {
	case <-c.release:
	case <-ctx.Done():
	}
	events := make(chan agent.Event)
	close(events)
	return events, nil
}

func (c *blockingClient) Cancel(_ context.Context, sessionID string) error {
	select {
	case c.cancelled <- sessionID:
	default:
	}
	return nil
}

func (c *blockingClient) Close() error { return nil }
