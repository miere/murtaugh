package nodehost_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/nodelink"
	"github.com/miere/murtaugh/internal/nodesocket"
	"github.com/miere/murtaugh/internal/tools/ask"
	"github.com/miere/murtaugh/internal/tools/plan"
)

func turnEmitter(turn *scriptedTurn) agent.TurnEmitter {
	return func(ev agent.Event) bool {
		select {
		case turn.events <- ev:
			return true
		case <-turn.ctx.Done():
			return false
		}
	}
}

func nativeCtx(turn *scriptedTurn, prompt agent.PromptRequest) context.Context {
	ctx := agent.WithTurnLocation(turn.ctx, agent.TurnLocation{ChannelID: prompt.Channel, ThreadTS: prompt.Thread, UserID: prompt.User})
	return agent.WithTurnEmitter(ctx, turnEmitter(turn))
}

func detachedCtx(turn *scriptedTurn, meta agent.SessionMetadata) context.Context {
	ctx := agent.WithTurnLocation(context.Background(), agent.TurnLocation{ChannelID: meta.ChannelID, ThreadTS: meta.ThreadTS})
	return agent.WithTurnEmitter(ctx, turnEmitter(turn))
}

func promptDefault(t *testing.T, rig *loopback) <-chan agent.Event {
	t.Helper()
	events, err := rig.sessions["default"].Prompt(context.Background(),
		agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"},
		agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4", UserID: nodeOwner},
		agent.PromptRequest{Text: "deploy it", Channel: "C1", Thread: "123.4", User: "U9"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	return events
}

// A question asked by a native tool on a node crosses as a display request and
// the answer the gateway gets from its human is what the tool hands the model.
func TestANativeToolsQuestionIsAnsweredThroughTheGateway(t *testing.T) {
	var script *scriptedAgent
	script = newScriptedAgent(func(turn *scriptedTurn) {
		out, err := ask.New(agent.TurnDisplay{}).Invoke(nativeCtx(turn, script.lastPrompt()), map[string]any{
			"title": "Deploy",
			"questions": []any{
				map[string]any{"header": "Env", "question": "Where?", "options": []any{"Staging", "Production"}},
				map[string]any{"question": "Regions?", "multiSelect": true, "options": []any{"US", "EU"}},
			},
		})
		if err != nil {
			turn.emit(agent.Event{Type: agent.EventError, Error: err})
			return
		}
		turn.emit(agent.Event{Type: agent.EventText, Text: out.(ask.Result).String()})
	})
	rig := dialLoopback(t, script)

	var reply string
	asked := 0
	for ev := range promptDefault(t, rig) {
		switch ev.Type {
		case agent.EventQuestion:
			asked++
			req := ev.Question.Request
			if req.Title != "Deploy" || len(req.Questions) != 2 || req.Questions[0].Header != "Env" || !req.Questions[1].MultiSelect {
				t.Fatalf("the question crossed as %+v", req)
			}
			ev.Question.Answer <- agent.DisplayAnswer{
				Outcome: agent.DisplayAnswered,
				Answers: map[string][]string{"q0": {"Production"}, "q1": {"US", "EU"}},
				UserID:  "U9",
			}
		case agent.EventText:
			reply += ev.Text
		case agent.EventError:
			t.Fatalf("the turn failed: %v", ev.Error)
		}
	}
	if asked != 1 {
		t.Fatalf("the gateway was asked %d questions, want 1", asked)
	}
	if !strings.Contains(reply, "Where?: Production") || !strings.Contains(reply, "Regions?: US, EU") {
		t.Fatalf("the model was handed %q", reply)
	}
}

// A turn torn down under an open question must answer it: a bridged call waits
// on its own context, which the turn ending does not cancel.
func TestATornDownTurnAnswersItsOpenPlan(t *testing.T) {
	meta := agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4", UserID: nodeOwner}
	outcome := make(chan string, 1)
	script := newScriptedAgent(func(turn *scriptedTurn) {
		go func() {
			out, err := plan.New(agent.TurnDisplay{}).Invoke(detachedCtx(turn, meta), map[string]any{"plan": "ship it"})
			if err != nil {
				outcome <- err.Error()
				return
			}
			outcome <- out.(plan.Result).String()
		}()
		<-turn.cancelled
	})
	rig := dialLoopback(t, script)

	manager := rig.sessions["default"]
	events := promptDefault(t, rig)
	for ev := range events {
		if ev.Type == agent.EventPlan {
			break
		}
	}
	sessionID, _ := manager.Lookup(agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"})
	cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = manager.Cancel(cancelCtx, sessionID)
	for range events {
	}

	select {
	case got := <-outcome:
		if !strings.Contains(got, "dismissed") {
			t.Fatalf("an abandoned plan came back as %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a plan open when its turn ended was never answered")
	}
}

// A headless run has no conversation, so the node's tool refuses before anything
// is sent, with the words it used when it ran on the gateway.
func TestAHeadlessQuestionIsRefusedOnTheNode(t *testing.T) {
	var script *scriptedAgent
	refused := make(chan error, 1)
	raised := make(chan agent.Event, 1)
	script = newScriptedAgent(func(turn *scriptedTurn) {
		ctx := agent.WithTurnEmitter(nativeCtx(turn, script.lastPrompt()), func(ev agent.Event) bool {
			raised <- ev
			return turnEmitter(turn)(ev)
		})
		_, err := ask.New(agent.TurnDisplay{}).Invoke(ctx, map[string]any{
			"question": "Ship it?", "options": []any{"Yes", "No"},
		})
		refused <- err
		turn.emit(agent.Event{Type: agent.EventComplete})
	})
	rig := dialLoopback(t, script)

	if err := delegatorFor(t, rig, config.AccessConfig{MainNode: "node-1"}, true).RunAndForget(context.Background(), "default", "the 03:00 job"); err != nil {
		t.Fatalf("the headless run failed: %v", err)
	}
	select {
	case err := <-refused:
		if err == nil || err.Error() != "Error: the ask tool only works inside a Slack conversation" {
			t.Fatalf("a headless ask was answered with %v", err)
		}
	default:
		t.Fatal("the headless run ended without its tool being called")
	}
	select {
	case ev := <-raised:
		t.Fatalf("a headless ask raised %s on the stream instead of refusing up front", ev.Type)
	default:
	}
}

// A node that raises a question on a headless turn anyway is refused by the
// gateway: nobody consumes a headless turn's cards, so it would wait forever.
func TestAHeadlessQuestionIsRefusedByTheGateway(t *testing.T) {
	answered := make(chan agent.DisplayAnswer, 1)
	script := newScriptedAgent(func(turn *scriptedTurn) {
		answer := make(chan agent.DisplayAnswer, 1)
		turnEmitter(turn)(agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionPrompt{
			Request: agent.QuestionRequest{Questions: []agent.Question{{Key: "q0", Question: "Ship it?", Options: []agent.QuestionOption{{Label: "Yes"}, {Label: "No"}}}}},
			Answer:  answer,
		}})
		select {
		case got := <-answer:
			answered <- got
		case <-time.After(10 * time.Second):
		}
		turn.emit(agent.Event{Type: agent.EventComplete})
	})
	rig := dialLoopback(t, script)

	if err := delegatorFor(t, rig, config.AccessConfig{MainNode: "node-1"}, true).RunAndForget(context.Background(), "default", "the 03:00 job"); err != nil {
		t.Fatalf("the headless run failed: %v", err)
	}
	select {
	case got := <-answered:
		if got.Outcome != agent.DisplayNoConversation {
			t.Fatalf("a headless question was answered with %+v", got)
		}
	default:
		t.Fatal("a question on a headless turn was never answered; the tool would wait until the job timed out")
	}
}

// A question raised after the gateway dropped its turn must still be answered,
// or the tool that raised it waits on nobody.
func TestAQuestionOnATurnTheGatewayDroppedIsAnswered(t *testing.T) {
	answered := make(chan agent.DisplayAnswer, 1)
	script := newScriptedAgent(func(turn *scriptedTurn) {
		turn.emit(agent.Event{Type: agent.EventText, Text: "working"})
		<-turn.cancelled
		answer := make(chan agent.DisplayAnswer, 1)
		turnEmitter(turn)(agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionPrompt{
			Request: agent.QuestionRequest{Questions: []agent.Question{{Key: "q0", Question: "Still there?", Options: []agent.QuestionOption{{Label: "Yes"}, {Label: "No"}}}}},
			Answer:  answer,
		}})
		select {
		case got := <-answer:
			answered <- got
		case <-time.After(10 * time.Second):
		}
	})
	rig := dialLoopback(t, script)

	ctx, cancel := context.WithCancel(context.Background())
	events, err := rig.sessions["default"].Prompt(ctx,
		agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"},
		agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4", UserID: nodeOwner},
		agent.PromptRequest{Text: "deploy it", Channel: "C1", Thread: "123.4"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if first := receiveEvent(t, events); first.Text != "working" {
		t.Fatalf("first event was %+v", first)
	}
	cancel()
	for range events {
	}

	select {
	case got := <-answered:
		if got.Outcome != agent.DisplayDismissed {
			t.Fatalf("a question on a dropped turn was answered with %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a question on a turn the gateway had dropped was never answered")
	}
}

// Outside a turn there is no stream to carry an answer back, so the node refuses
// a question itself rather than sending one nobody can resolve.
func TestABackgroundQuestionIsRefusedOnTheNode(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}))

	answer := make(chan agent.DisplayAnswer, 1)
	rig.background.Handle("node-session-1", agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionPrompt{
		Request: agent.QuestionRequest{Questions: []agent.Question{{Key: "q0", Question: "Ship it?", Options: []agent.QuestionOption{{Label: "Yes"}, {Label: "No"}}}}},
		Answer:  answer,
	}})

	select {
	case got := <-answer:
		if got.Outcome != agent.DisplayNoConversation {
			t.Fatalf("a background question was answered with %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a background question was never answered; the tool that raised it is parked")
	}
	select {
	case got := <-rig.notices:
		t.Fatalf("a background question reached the gateway's router as %+v", got.event)
	case <-time.After(200 * time.Millisecond):
	}
}

// A node that sends a question outside any turn anyway is refused by the gateway,
// and nothing about it reaches the background router.
func TestABackgroundQuestionIsNotRenderedByTheGateway(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}))
	node := attachRaw(t, rig, "node-raw")

	node.send(t, mustBackground(t, "node-session-1", agentwire.Event{Type: agentwire.EventQuestion, Question: &agentwire.QuestionRequest{
		ID:        "question-1",
		Questions: []agentwire.Question{{Key: "q0", Question: "Ship it?", Options: []agentwire.QuestionOption{{Label: "Yes"}, {Label: "No"}}}},
	}}))

	select {
	case got := <-node.answers:
		if got.ID != "question-1" || got.Outcome != string(agent.DisplayNoConversation) {
			t.Fatalf("the gateway answered %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway never answered a background question")
	}
	select {
	case got := <-rig.notices:
		t.Fatalf("a background question reached the gateway's router as %+v", got.event)
	case <-time.After(200 * time.Millisecond):
	}
}

type rawNode struct {
	link    *nodelink.Link
	answers chan agentwire.DisplayAnswer
}

func (n *rawNode) send(t *testing.T, msg agentwire.Message) {
	t.Helper()
	raw, err := msg.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := n.link.Send(ctx, raw); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func mustBackground(t *testing.T, sessionID string, ev agentwire.Event) agentwire.Message {
	t.Helper()
	msg, err := agentwire.BackgroundEvent(sessionID, ev)
	if err != nil {
		t.Fatalf("background frame: %v", err)
	}
	return msg
}

func attachRaw(t *testing.T, rig *loopback, nodeID string) *rawNode {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	minted := mintInto(t, rig.store, nodeID)
	conn, err := nodesocket.Dial(ctx, "ws://"+rig.addr, nodesocket.DialOptions{Token: minted.Token})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	node := &rawNode{answers: make(chan agentwire.DisplayAnswer, 4)}
	ready := make(chan struct{})
	node.link = nodelink.New(conn, nodelink.Options{
		Logger:      testLogger(),
		WindowBytes: nodesocket.DefaultWindowBytes,
		Handler: func(raw []byte) error {
			msg, err := agentwire.DecodeMessage(raw)
			if err != nil {
				return nil
			}
			switch {
			case msg.Kind == agentwire.MessageRequest && msg.Method == agentwire.MethodInitialize:
				go func() {
					<-ready
					reply, _ := agentwire.Result(msg.ID, agentwire.InitializeResult{})
					node.send(t, reply)
				}()
			case msg.Kind == agentwire.MessageAnswer:
				var answer agentwire.DisplayAnswer
				if msg.Into(&answer) == nil {
					node.answers <- answer
				}
			}
			return nil
		},
	})
	close(ready)
	t.Cleanup(func() { _ = node.link.Close() })
	waitFor(t, "the raw node to attach", func() bool {
		for _, n := range rig.host.Nodes() {
			if n.NodeID == nodeID {
				return true
			}
		}
		return false
	})
	return node
}
