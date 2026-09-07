package remote

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/nodelink"
)

// The five methods of agent.Client, both optional surfaces the session manager
// asserts for on the client, and the two liveness rules the gateway and the
// delegate runner depend on. Every test drives a real envelope in both
// directions.

func TestInitializeResolvesInterruptibility(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name  string
		says  *bool
		wants bool
	}{
		{"a node whose agent can be interrupted", &yes, true},
		{"a node whose agent cannot", &no, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _, _ := dial(t, nodeConfig{interruptible: tc.says}, Options{})
			if err := client.Initialize(context.Background()); err != nil {
				t.Fatalf("initialize: %v", err)
			}
			if got := client.SupportsCancel(context.Background()); got != tc.wants {
				t.Fatalf("SupportsCancel = %v, want %v", got, tc.wants)
			}
		})
	}
}

// The degradation, proven to be visible. A node that says nothing about
// interruptibility is treated as interruptible — the session manager's own
// answer for an unresolved probe — and says so where an operator can see it.
// The alternative, a plain bool decoding to false, would silently stop new
// messages from interrupting an in-flight turn.
func TestUnreportedInterruptibilityDegradesVisibly(t *testing.T) {
	client, _, logs := dial(t, nodeConfig{interruptible: nil}, Options{})
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if !client.SupportsCancel(context.Background()) {
		t.Fatal("an unreported capability was read as a denial")
	}
	if !strings.Contains(logs.String(), "did not report whether its agent can be interrupted") {
		t.Fatalf("the degradation was silent; log was:\n%s", logs.String())
	}
}

func TestSupportsCancelBeforeInitializeIsInterruptible(t *testing.T) {
	no := false
	client, _, _ := dial(t, nodeConfig{interruptible: &no}, Options{})
	if !client.SupportsCancel(context.Background()) {
		t.Fatal("an unresolved capability disabled interrupts")
	}
}

func TestNewSessionCarriesTheMetadata(t *testing.T) {
	client, node, _ := dial(t, nodeConfig{sessionID: "session-7"}, Options{})
	meta := agent.SessionMetadata{
		TeamID: "T1", ChannelID: "C1", ThreadTS: "1700000000.000100",
		UserID: "U1", Source: "slack", Surface: "canvas", CanvasID: "F1",
	}
	session, err := client.NewSession(context.Background(), meta)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if session.ID != "session-7" {
		t.Fatalf("session id = %q", session.ID)
	}
	if got := node.seenMetadata().Decode(); got != meta {
		t.Fatalf("node saw %+v, want %+v", got, meta)
	}
}

// A node that answers new_session with no id is malformed, and passing the
// empty id on would turn every later call into one naming a session that does
// not exist — a turn that fails somewhere further away, with a worse message.
func TestNewSessionRefusesANodeThatReturnsNoID(t *testing.T) {
	client, _, _ := dial(t, nodeConfig{noSessionID: true}, Options{})
	session, err := client.NewSession(context.Background(), agent.SessionMetadata{ChannelID: "C1"})
	if err == nil {
		t.Fatalf("a session with no id was accepted as %+v", session)
	}
	if session.ID != "" {
		t.Fatalf("a refused session came back with id %q", session.ID)
	}
}

// The stream is registered BEFORE the request goes out. The node here emits its
// first event before it accepts the prompt, which is legal and is what a node
// that starts working immediately looks like. Register after the call returns
// and deliverEvent finds no stream, drops the event on the floor, and the turn
// silently loses its opening — with nothing failing anywhere.
func TestPromptRegistersTheStreamBeforeTheRequestGoesOut(t *testing.T) {
	eager := agentwire.Event{Type: agentwire.EventText, Text: "starting before I answered you"}
	client, node, _ := dial(t, nodeConfig{eagerEvent: &eager}, Options{})

	events, err := client.Prompt(context.Background(), "session-42", agent.PromptRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	stream := receive(t, "the accepted stream", node.streams)
	node.emit(stream, agentwire.Event{Type: agentwire.EventComplete, StopReason: "end_turn"})
	node.end(stream)

	first := receive(t, "the event the node emitted before accepting the prompt", events)
	if first.Type != agent.EventText || first.Text != eager.Text {
		t.Fatalf("first event = %+v, want the pre-acceptance text %q; an event that arrived "+
			"before the acceptance was dropped", first, eager.Text)
	}
}

func TestPromptStreamsEventsInOrderAndEndsTheChannel(t *testing.T) {
	client, node, _ := dial(t, nodeConfig{}, Options{})
	events, err := client.Prompt(context.Background(), "session-42", agent.PromptRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	body := receive(t, "the prompt to reach the node", node.prompts)
	if body.Prompt.Text != "hello" || body.SessionID != "session-42" {
		t.Fatalf("node saw %+v", body)
	}
	stream := receive(t, "the accepted stream", node.streams)

	node.emit(stream, agentwire.Event{Type: agentwire.EventText, Text: "one "})
	node.emit(stream, agentwire.Event{Type: agentwire.EventTask, Task: &agentwire.Task{ID: "t1", Title: "grep", Status: agentwire.TaskStatusInProgress}})
	node.emit(stream, agentwire.Event{Type: agentwire.EventText, Text: "two"})
	node.emit(stream, agentwire.Event{Type: agentwire.EventComplete, StopReason: "end_turn"})
	node.end(stream)

	var text strings.Builder
	var kinds []agent.EventType
	for ev := range events {
		kinds = append(kinds, ev.Type)
		if ev.Type == agent.EventText {
			text.WriteString(ev.Text)
		}
		if ev.Type == agent.EventTask && ev.Task.Title != "grep" {
			t.Fatalf("task came through as %+v", ev.Task)
		}
	}
	if text.String() != "one two" {
		t.Fatalf("reply reassembled as %q; the stream is out of order or lossy", text.String())
	}
	want := []agent.EventType{agent.EventText, agent.EventTask, agent.EventText, agent.EventComplete}
	if len(kinds) != len(want) {
		t.Fatalf("stream delivered %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("stream delivered %v, want %v", kinds, want)
		}
	}
}

// Both consumers read Prompt's error synchronously, before any rendering
// starts, so a refusal must be the call's answer and not the first event on an
// already-opened stream.
func TestPromptRejectionIsReturnedSynchronously(t *testing.T) {
	client, _, _ := dial(t, nodeConfig{rejectPrompt: errors.New("no session on this node")}, Options{})
	events, err := client.Prompt(context.Background(), "session-42", agent.PromptRequest{Text: "hello"})
	if err == nil {
		t.Fatal("a rejected prompt returned no error")
	}
	if events != nil {
		t.Fatal("a rejected prompt opened a stream")
	}
	if !strings.Contains(err.Error(), "no session on this node") {
		t.Fatalf("rejection lost its text: %v", err)
	}
}

// The contract chat_handler.go and delegate.go both depend on: `cancel(); for
// range events {}` completes. Abandoning that channel would stall event
// delivery for every other conversation, which is why they drain rather than
// walk away — so the channel has to close.
func TestCancelledContextClosesTheChannelAndStopsTheNode(t *testing.T) {
	client, node, _ := dial(t, nodeConfig{}, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	events, err := client.Prompt(ctx, "session-42", agent.PromptRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	stream := receive(t, "the accepted stream", node.streams)
	node.emit(stream, agentwire.Event{Type: agentwire.EventText, Text: "working"})

	drained := make(chan struct{})
	go func() {
		cancel()
		for range events {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("the turn's channel never closed; the consumer would be wedged")
	}

	// In process the cancelled context reaches the backend. Across a link it
	// reaches nothing, so the turn has to be cancelled explicitly.
	if got := receive(t, "the node to be told to stop", node.cancels); got != "session-42" {
		t.Fatalf("node was told to cancel %q", got)
	}
}

func TestCancelIsCarriedAndHonoursItsDeadline(t *testing.T) {
	client, node, _ := dial(t, nodeConfig{}, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Cancel(ctx, "session-42"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := receive(t, "the cancel to reach the node", node.cancels); got != "session-42" {
		t.Fatalf("node was told to cancel %q", got)
	}
}

// A node that stops answering must not park the idle path: chat_handler cancels
// under five seconds while holding a wedged turn open.
func TestCancelReturnsWhenTheNodeDoesNotAnswer(t *testing.T) {
	gatewaySide, nodeSide := nodelink.Pipe(32)
	client := New(gatewaySide, Options{Logger: discardLogger()})
	// The far end first: a transport with nobody on it never answers, and the
	// goodbye in Close would otherwise sit out its whole timeout.
	t.Cleanup(func() { _ = nodeSide.Close(); _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := client.Cancel(ctx, "session-42")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel returned %v, want the deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("cancel took %s to honour a 100ms deadline", elapsed)
	}
}

// CloseSession is called while SessionManager.mu is held, with no context and
// no error return. If it waited for the wire, every conversation on this agent
// would queue behind one network call.
func TestCloseSessionReturnsWhileTheWriteIsBlocked(t *testing.T) {
	// A pipe with no slack and nothing reading: the sender goroutine's write
	// blocks on the first frame and stays blocked.
	gatewaySide, nodeSide := nodelink.Pipe(0)
	client := New(gatewaySide, Options{Logger: discardLogger()})
	// The transport is released before the client, because a write parked on a
	// wedged transport is unblocked by the transport, not by a context — see
	// nodelink.Conn.
	t.Cleanup(func() { _ = nodeSide.Close(); _ = client.Close() })

	done := make(chan struct{})
	go func() {
		for i := 0; i < 5; i++ {
			client.CloseSession("session-42")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("CloseSession waited for the wire; it runs under the session manager's mutex")
	}
}

func TestCloseSessionReachesTheNode(t *testing.T) {
	client, node, _ := dial(t, nodeConfig{}, Options{})
	client.CloseSession("session-42")
	if got := receive(t, "the node to release the session", node.released); got != "session-42" {
		t.Fatalf("node released %q", got)
	}
}

func TestCloseSaysGoodbyeAndTearsDown(t *testing.T) {
	client, node, _ := dial(t, nodeConfig{}, Options{})
	if err := client.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	receive(t, "the node to be told the client is closing", node.goodbyes)
	if _, err := client.NewSession(context.Background(), agent.SessionMetadata{}); err == nil {
		t.Fatal("a closed client still opened a session")
	}
}

// An event addressed to a session rather than to a request is the background
// path: a subagent completing after its turn ended. It must reach the router
// and never a turn's stream.
func TestBackgroundEventsReachTheSinkNotTheStream(t *testing.T) {
	type delivered struct {
		sessionID string
		event     agent.Event
	}
	sink := make(chan delivered, 4)
	client, node, _ := dial(t, nodeConfig{}, Options{
		Background: func(sessionID string, ev agent.Event) { sink <- delivered{sessionID, ev} },
	})

	events, err := client.Prompt(context.Background(), "session-42", agent.PromptRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	stream := receive(t, "the accepted stream", node.streams)

	node.emitBackground("session-99", agentwire.Event{Type: agentwire.EventText, Text: "late reply"})
	got := receive(t, "the background event", sink)
	if got.sessionID != "session-99" || got.event.Text != "late reply" {
		t.Fatalf("background sink saw %+v", got)
	}

	node.end(stream)
	for ev := range events {
		t.Fatalf("the background event landed on a turn's stream: %+v", ev)
	}
}

// A permission request is the one event that is half of a request/response
// pair. The answer has to travel back under the id the node minted, or the
// goroutine blocked on it never wakes.
func TestPermissionAnswerTravelsBack(t *testing.T) {
	client, node, _ := dial(t, nodeConfig{}, Options{})
	events, err := client.Prompt(context.Background(), "session-42", agent.PromptRequest{Text: "rm -rf"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	stream := receive(t, "the accepted stream", node.streams)
	node.emit(stream, agentwire.Event{Type: agentwire.EventPermission, Permission: &agentwire.PermissionRequest{
		ID: "perm-1", Gate: agentwire.GateTool, ToolKind: "terminal", ToolTitle: "rm -rf /", PolicyOwned: true,
	}})

	ev := receive(t, "the permission event", events)
	if ev.Type != agent.EventPermission || ev.Permission == nil {
		t.Fatalf("permission arrived as %+v", ev)
	}
	if ev.Permission.Request.ToolTitle != "rm -rf /" || !ev.Permission.Request.PolicyOwned {
		t.Fatalf("permission request came through as %+v", ev.Permission.Request)
	}
	ev.Permission.Decision <- agent.PermissionDeny

	answer := receive(t, "the decision to reach the node", node.decisions)
	if answer.ID != "perm-1" || answer.OptionID != agent.PermissionDeny {
		t.Fatalf("node received %+v", answer)
	}
	node.end(stream)
	for range events {
	}
}

// A node that dies mid-turn must produce a visible failure and a closed
// channel, not a stream nobody ever closes.
func TestLinkDeathFailsOpenTurns(t *testing.T) {
	client, node, _ := dial(t, nodeConfig{}, Options{})
	events, err := client.Prompt(context.Background(), "session-42", agent.PromptRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	stream := receive(t, "the accepted stream", node.streams)
	node.emit(stream, agentwire.Event{Type: agentwire.EventText, Text: "half a sen"})
	if ev := receive(t, "the first event", events); ev.Text != "half a sen" {
		t.Fatalf("first event = %+v", ev)
	}
	_ = node.link.Close()

	var last agent.Event
	for ev := range events {
		last = ev
	}
	if last.Type != agent.EventError || last.Error == nil {
		t.Fatalf("a dead node ended the turn with %+v; the user would see silence", last)
	}
	if !errors.Is(last.Error, nodelink.ErrLinkClosed) && !errors.Is(last.Error, io.EOF) {
		t.Fatalf("turn failed with %v, which names no cause", last.Error)
	}
}

// The loss-injection case, at the level a user would feel it: one event frame
// never arrives. The turn must end with the gap named — comparable by identity,
// not by prose — rather than with a hole in a sentence.
func TestDroppedEventFailsTheTurnWithTheGap(t *testing.T) {
	lost := func(payload []byte) bool {
		msg, err := agentwire.DecodeMessage(payload)
		if err != nil || msg.Kind != agentwire.MessageEvent {
			return false
		}
		var ev agentwire.Event
		if err := msg.Into(&ev); err != nil {
			return false
		}
		return ev.Text == "lost"
	}
	client, node, _ := dial(t, nodeConfig{dropFrame: lost}, Options{})
	events, err := client.Prompt(context.Background(), "session-42", agent.PromptRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	stream := receive(t, "the accepted stream", node.streams)

	node.emit(stream, agentwire.Event{Type: agentwire.EventText, Text: "kept"})
	node.emit(stream, agentwire.Event{Type: agentwire.EventText, Text: "lost"})
	node.emit(stream, agentwire.Event{Type: agentwire.EventText, Text: "after"})

	var seen []string
	var failure error
	for ev := range events {
		switch ev.Type {
		case agent.EventText:
			seen = append(seen, ev.Text)
		case agent.EventError:
			failure = ev.Error
		}
	}
	if failure == nil {
		t.Fatal("a dropped frame was swallowed: the turn ended without an error")
	}
	if !errors.Is(failure, nodelink.ErrSequenceGap) {
		t.Fatalf("the turn failed with %v, which does not name the gap", failure)
	}
	if len(seen) != 1 || seen[0] != "kept" {
		t.Fatalf("the turn delivered %v; nothing after the gap may be rendered", seen)
	}
}
