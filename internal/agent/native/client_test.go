package native

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/llm"
	"github.com/miere/murtaugh/internal/tools"
)

// newTestClient builds a Client wired to a fake provider, bypassing Build (which
// would need real credentials). Initialize still runs to resolve an (empty)
// toolset and construct the loop.
func newTestClient(prov *fakeProvider) *Client {
	return &Client{
		provider:       prov,
		model:          "test-model",
		maxTurns:       10,
		cacheRetention: defaultCacheRetention,
		now:            func() time.Time { return time.Date(2026, 6, 17, 18, 51, 0, 0, time.UTC) },
		logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions:       make(map[string]*nativeSession),
		cancels:        make(map[string]*inflight),
	}
}

func drain(ch <-chan agent.Event) []agent.Event {
	var evs []agent.Event
	for ev := range ch {
		evs = append(evs, ev)
	}
	return evs
}

func TestClient_PromptStreamsTextAndCompletes(t *testing.T) {
	prov := &fakeProvider{turns: []scriptedTurn{{text: "hello there", stopReason: "end_turn"}}}
	c := newTestClient(prov)
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sess, err := c.NewSession(ctx, agent.SessionMetadata{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	ch, err := c.Prompt(ctx, sess.ID, agent.PromptRequest{Text: "hi"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	evs := drain(ch)

	var text strings.Builder
	var sawComplete bool
	for _, ev := range evs {
		switch ev.Type {
		case agent.EventText:
			text.WriteString(ev.Text)
		case agent.EventComplete:
			sawComplete = true
		}
	}
	if text.String() != "hello there" {
		t.Errorf("streamed text = %q, want %q", text.String(), "hello there")
	}
	if !sawComplete {
		t.Error("expected an EventComplete")
	}
}

func TestClient_UnknownSession(t *testing.T) {
	c := newTestClient(&fakeProvider{})
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := c.Prompt(context.Background(), "nope", agent.PromptRequest{Text: "x"}); err == nil {
		t.Fatal("expected error for unknown session")
	}
}

// TestClient_HistoryFoldsIntoSingleUserMessage proves the cold-start backfill
// (req.History) is merged into the SAME user message rather than appended as a
// second one — so the array never gains consecutive user messages.
func TestClient_HistoryFoldsIntoSingleUserMessage(t *testing.T) {
	prov := &fakeProvider{turns: []scriptedTurn{{text: "ok", stopReason: "end_turn"}}}
	c := newTestClient(prov)
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sess, _ := c.NewSession(ctx, agent.SessionMetadata{})
	ch, err := c.Prompt(ctx, sess.ID, agent.PromptRequest{
		Text:    "current question",
		History: "earlier thread backstory",
	})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	drain(ch)

	if len(prov.requests) == 0 {
		t.Fatal("provider received no request")
	}
	msgs := prov.requests[0].Messages
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 message (folded), got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" {
		t.Errorf("first message role = %q, want user", msgs[0].Role)
	}
	// The single user message folds the volatile context, the history backfill,
	// and the question together — never a second message.
	for _, want := range []string{"earlier thread backstory", "current question", "It is currently 2026-06-17"} {
		if !strings.Contains(msgs[0].Text, want) {
			t.Errorf("folded user message missing %q: %q", want, msgs[0].Text)
		}
	}
	// The system prompt stays STATIC — no volatile context — so it can be cached.
	if strings.Contains(prov.requests[0].System, "It is currently") {
		t.Errorf("volatile context leaked into System (should be on the user turn): %q", prov.requests[0].System)
	}
}

func TestResolveCacheRetention(t *testing.T) {
	cases := map[string]string{
		"":     defaultCacheRetention,
		"off":  "",
		"none": "",
		"5m":   "5m",
		"1h":   "1h",
		"long": "long",
	}
	for in, want := range cases {
		if got := resolveCacheRetention(in); got != want {
			t.Errorf("resolveCacheRetention(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClient_SupportsCancel(t *testing.T) {
	c := newTestClient(&fakeProvider{})
	if !c.SupportsCancel(context.Background()) {
		t.Error("native client should report SupportsCancel = true")
	}
}

// TestClient_SystemStaticAcrossTurns is the caching regression: across turns
// (with a changing clock) the system prompt must stay byte-identical so the
// provider can cache it, the volatile timestamp must ride the user turn instead,
// and cache retention must be requested.
func TestClient_SystemStaticAcrossTurns(t *testing.T) {
	prov := &fakeProvider{turns: []scriptedTurn{
		{text: "a", stopReason: "end_turn"},
		{text: "b", stopReason: "end_turn"},
	}}
	c := newTestClient(prov)
	c.systemPrompt = "You are a test bot."
	times := []time.Time{
		time.Date(2026, 6, 17, 18, 51, 0, 0, time.UTC),
		time.Date(2026, 6, 17, 19, 30, 0, 0, time.UTC),
	}
	i := 0
	c.now = func() time.Time { tt := times[i%len(times)]; i++; return tt }

	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sess, _ := c.NewSession(ctx, agent.SessionMetadata{})
	for turn := 0; turn < 2; turn++ {
		ch, err := c.Prompt(ctx, sess.ID, agent.PromptRequest{Text: "hi", Channel: "C1"})
		if err != nil {
			t.Fatalf("Prompt turn %d: %v", turn, err)
		}
		drain(ch)
	}

	if len(prov.requests) < 2 {
		t.Fatalf("want at least 2 requests, got %d", len(prov.requests))
	}
	if prov.requests[0].System != prov.requests[1].System {
		t.Errorf("system changed across turns; not cache-stable:\n1=%q\n2=%q", prov.requests[0].System, prov.requests[1].System)
	}
	if prov.requests[0].System != "You are a test bot." {
		t.Errorf("system = %q, want the static base only", prov.requests[0].System)
	}
	if prov.requests[0].CacheRetention != defaultCacheRetention {
		t.Errorf("CacheRetention = %q, want %q", prov.requests[0].CacheRetention, defaultCacheRetention)
	}
	// Each turn's newest user message carries that turn's timestamp.
	last0 := prov.requests[0].Messages[len(prov.requests[0].Messages)-1].Text
	last1 := prov.requests[1].Messages[len(prov.requests[1].Messages)-1].Text
	if !strings.Contains(last0, "18:51") || !strings.Contains(last1, "19:30") {
		t.Errorf("per-turn timestamps not on the user turn:\nturn0=%q\nturn1=%q", last0, last1)
	}
}

type emittingTool struct {
	emitted chan bool
}

func (t *emittingTool) Name() string                    { return "raise" }
func (t *emittingTool) Description() string             { return "raises an event on the turn" }
func (t *emittingTool) InputSchema() *jsonschema.Schema { return &jsonschema.Schema{Type: "object"} }
func (t *emittingTool) Invoke(ctx context.Context, _ map[string]any) (any, error) {
	emit, ok := agent.TurnEmitterFromContext(ctx)
	t.emitted <- ok && emit(agent.Event{Type: agent.EventStatus, Text: "from the tool"})
	return "done", nil
}

// A native tool reaches the user through the turn's own stream, as a bridged
// tool does on acp and claude_code, so ask and present_plan work the same on all three.
func TestClient_ToolsCanRaiseEventsOnTheirTurn(t *testing.T) {
	prov := &fakeProvider{turns: []scriptedTurn{
		{toolCalls: []llm.ToolCall{{ID: "c1", Name: "raise", Arguments: rawArgs(t, map[string]any{})}}},
		{text: "ok", stopReason: "end_turn"},
	}}
	c := newTestClient(prov)
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	tool := &emittingTool{emitted: make(chan bool, 1)}
	c.loop = NewLoop(prov, "test-model", []tools.Tool{tool}, 10)
	sess, err := c.NewSession(ctx, agent.SessionMetadata{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	ch, err := c.Prompt(ctx, sess.ID, agent.PromptRequest{Text: "hi"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	evs := drain(ch)

	if !<-tool.emitted {
		t.Fatal("the tool found no emitter on its context, or its event was refused")
	}
	raised, completed := -1, -1
	for i, ev := range evs {
		if ev.Type == agent.EventStatus && ev.Text == "from the tool" {
			raised = i
		}
		if ev.Type == agent.EventTask && ev.Task.ID == "c1" && ev.Task.Status == agent.TaskStatusComplete {
			completed = i
		}
	}
	if raised < 0 || completed < 0 || raised > completed {
		t.Fatalf("the tool's event is at %d and its completion at %d; it must reach the stream while the tool runs", raised, completed)
	}
}
