package nodeserve

import (
	"context"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/nodelink"
)

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

type pipePeer struct {
	link      *nodelink.Link
	responses chan agentwire.Message
}

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

func (p *pipePeer) response(t *testing.T) agentwire.Message {
	t.Helper()
	select {
	case msg := <-p.responses:
		return msg
	case <-time.After(10 * time.Second):
		t.Fatal("the node never answered")
		return agentwire.Message{}
	}
}

func serveOverPipe(t *testing.T, client agent.Client) *pipePeer {
	t.Helper()
	return serveOverPipeWith(t, client, Options{})
}

func serveOverPipeWith(t *testing.T, client agent.Client, opts Options) *pipePeer {
	t.Helper()
	gatewayConn, nodeConn := nodelink.Pipe(8)
	ctx, cancel := context.WithCancel(context.Background())

	opts.Logger = discardLogger()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = Serve(ctx, nodeConn, client, opts)
	}()

	responses := make(chan agentwire.Message, 16)
	link := nodelink.New(gatewayConn, nodelink.Options{
		Handler: func(raw []byte) error {
			if msg, err := agentwire.DecodeMessage(raw); err == nil && msg.Kind == agentwire.MessageResponse {
				select {
				case responses <- msg:
				default:
				}
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
	return &pipePeer{link: link, responses: responses}
}

// A blank answer makes the gateway warn and guess, and an override the node ignored would let a
// profile that says "never interrupt" be interrupted anyway.
func TestANodeAlwaysSaysWhetherItsAgentCanBeInterrupted(t *testing.T) {
	no, yes := false, true
	for _, tc := range []struct {
		name     string
		client   agent.Client
		override *bool
		want     bool
	}{
		{"nothing to go on", &blockingClient{}, nil, true},
		{"the agent is asked", &probingClient{cancels: false}, nil, false},
		{"the override beats the agent", &probingClient{cancels: false}, &yes, true},
		{"the override with nothing to ask", &blockingClient{}, &no, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := serveOverPipeWith(t, tc.client, Options{Interruptible: tc.override})
			peer.request(t, "i1", agentwire.MethodInitialize, agentwire.Empty{})

			msg := peer.response(t)
			if fault := msg.Fault(); fault != nil {
				t.Fatalf("initialize failed: %v", fault)
			}
			var result agentwire.InitializeResult
			if err := msg.Into(&result); err != nil {
				t.Fatalf("read the initialize result: %v", err)
			}
			if result.Interruptible == nil {
				t.Fatal("the node did not say whether its agent can be interrupted")
			}
			if *result.Interruptible != tc.want {
				t.Errorf("the node said interruptible=%v, want %v", *result.Interruptible, tc.want)
			}
		})
	}
}

type probingClient struct {
	blockingClient
	cancels bool
}

func (c *probingClient) SupportsCancel(context.Context) bool { return c.cancels }

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
