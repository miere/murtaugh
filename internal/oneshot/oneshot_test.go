package oneshot_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/oneshot"
)

// A non-ephemeral session resumes the previous run's transcript, and a
// non-headless one raises an approval card nobody can answer.
func TestADelegationOpensAnEphemeralHeadlessSession(t *testing.T) {
	client := &recordingClient{reply: "done"}
	if _, err := oneshot.Drive(context.Background(), client, oneshot.Request{Agent: "default", Prompt: "go"}); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if !client.meta.Ephemeral {
		t.Fatal("the session was not ephemeral; every delegation would derive the same id and resume the last one's transcript")
	}
	if !client.meta.Headless {
		t.Fatal("the session was not headless; a node builds every agent with its approval gate and would raise a card for a 03:00 job")
	}
	if client.meta.Source != "delegate" {
		t.Fatalf("the session's source is %q", client.meta.Source)
	}
	if client.meta.ChannelID != "" || client.meta.ThreadTS != "" || client.meta.UserID != "" {
		t.Fatalf("the session named a Slack location: %+v", client.meta)
	}
}

// A caller may parse the reply as JSON, so progress events must not leak into it.
func TestOnlyTheReplyTextIsCaptured(t *testing.T) {
	client := &recordingClient{events: []agent.Event{
		{Type: agent.EventStatus, Text: "compacting…"},
		{Type: agent.EventText, Text: `{"text":`},
		{Type: agent.EventText, Text: `"hi"}`},
		{Type: agent.EventComplete},
	}}
	out, err := oneshot.Drive(context.Background(), client, oneshot.Request{Agent: "default", Prompt: "go"})
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if out != `{"text":"hi"}` {
		t.Fatalf("captured %q; progress and meta events must not pollute output a caller parses", out)
	}
	if _, err := oneshot.ExpectJSON(out, "default", nil); err != nil {
		t.Fatalf("the captured output was not usable as JSON: %v", err)
	}
}

func TestNonJSONOutputIsASkip(t *testing.T) {
	if _, err := oneshot.ExpectJSON("sure, here you go", "default", nil); !errors.Is(err, agent.ErrNonJSONOutput) {
		t.Fatalf("a prose answer produced %v", err)
	}
}

func TestASilentAgentIsCutLoose(t *testing.T) {
	client := &recordingClient{hang: true}
	_, err := oneshot.Drive(context.Background(), client, oneshot.Request{
		Agent: "default", Prompt: "go", IdleTimeout: 50 * time.Millisecond,
	})
	if err == nil || !strings.Contains(err.Error(), "went idle") {
		t.Fatalf("a silent agent produced %v", err)
	}
	select {
	case <-client.prompted:
	case <-time.After(time.Second):
		t.Fatal("the prompt context was never cancelled, so the in-flight request is still blocked")
	}
}

// The partial text is kept because a caller needs it to say anything useful
// about a half-finished job.
func TestAnErrorEventEndsTheTurn(t *testing.T) {
	client := &recordingClient{events: []agent.Event{
		{Type: agent.EventText, Text: "half "},
		{Type: agent.EventError, Error: errors.New("the model gave up")},
	}}
	out, err := oneshot.Drive(context.Background(), client, oneshot.Request{Agent: "default", Prompt: "go"})
	if err == nil || !strings.Contains(err.Error(), "the model gave up") {
		t.Fatalf("an error event produced %v", err)
	}
	if out != "half " {
		t.Fatalf("the partial output was lost: %q", out)
	}
}

// Over the link the client is the node's long-lived connection, so a session
// left open here leaks until the node restarts.
func TestEveryWayATurnEndsClosesItsSession(t *testing.T) {
	for name, tc := range map[string]struct {
		client *recordingClient
		req    oneshot.Request
	}{
		"completed": {client: &recordingClient{reply: "done"}},
		"errored": {client: &recordingClient{events: []agent.Event{
			{Type: agent.EventError, Error: errors.New("the model gave up")},
		}}},
		"channel closed with no completion": {client: &recordingClient{events: []agent.Event{
			{Type: agent.EventText, Text: "half "},
		}}},
		"went idle": {
			client: &recordingClient{hang: true},
			req:    oneshot.Request{IdleTimeout: 50 * time.Millisecond},
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := tc.req
			req.Agent, req.Prompt = "default", "go"
			_, _ = oneshot.Drive(context.Background(), tc.client, req)
			if got := tc.client.closedSessions(); len(got) != 1 || got[0] != "s1" {
				t.Fatalf("Drive opened session s1 and closed %v. Over a link that session lives until the node restarts — "+
					"one per scheduled job, per workflow trigger and per pasted link", got)
			}
		})
	}
}

type recordingClient struct {
	reply    string
	events   []agent.Event
	hang     bool
	meta     agent.SessionMetadata
	prompted chan struct{}

	mu     sync.Mutex
	closed []string
}

func (c *recordingClient) CloseSession(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = append(c.closed, id)
}

func (c *recordingClient) closedSessions() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.closed)
}

func (c *recordingClient) Initialize(context.Context) error { return nil }

func (c *recordingClient) NewSession(_ context.Context, meta agent.SessionMetadata) (agent.Session, error) {
	c.meta = meta
	return agent.Session{ID: "s1"}, nil
}

func (c *recordingClient) Prompt(ctx context.Context, _ string, _ agent.PromptRequest) (<-chan agent.Event, error) {
	events := make(chan agent.Event, 8)
	c.prompted = make(chan struct{})
	done := c.prompted
	go func() {
		defer close(events)
		defer close(done)
		if c.hang {
			<-ctx.Done()
			return
		}
		script := c.events
		if script == nil {
			script = []agent.Event{{Type: agent.EventText, Text: c.reply}, {Type: agent.EventComplete}}
		}
		for _, ev := range script {
			events <- ev
		}
	}()
	return events, nil
}

func (c *recordingClient) Cancel(context.Context, string) error { return nil }
func (c *recordingClient) Close() error                         { return nil }
