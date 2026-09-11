package nodehost_test

import (
	"context"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
)

func signInPrompt() *agent.SignInPrompt {
	return &agent.SignInPrompt{
		Request: agent.SignInRequest{Tool: "gcp-mcp", Profile: "gcloud", URL: "https://accounts.example.com/o/oauth2?x=1", NeedsCode: true},
		Answer:  make(chan agent.DisplayAnswer, 2),
	}
}

func awaitAnswer(t *testing.T, answers <-chan agent.DisplayAnswer, what string) agent.DisplayAnswer {
	t.Helper()
	select {
	case got := <-answers:
		return got
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never reached the node", what)
		return agent.DisplayAnswer{}
	}
}

// The code the owner types crosses to the node, and the node's word on how the
// sign-in ended reaches the gateway before the reply that follows it.
func TestASignInCarriesTheCodeToTheNodeAndSettlesInOrder(t *testing.T) {
	codes := make(chan agent.DisplayAnswer, 1)
	script := newScriptedAgent(func(turn *scriptedTurn) {
		prompt := signInPrompt()
		if !turnEmitter(turn)(agent.Event{Type: agent.EventSignIn, SignIn: prompt}) {
			return
		}
		select {
		case got := <-prompt.Answer:
			codes <- got
		case <-time.After(10 * time.Second):
			return
		}
		turnEmitter(turn)(agent.Event{Type: agent.EventSignInSettled, SignInSettled: &agent.SignInSettled{Prompt: prompt, State: agent.SignInSuccess}})
		turn.emit(agent.Event{Type: agent.EventText, Text: "signed in"})
		turn.emit(agent.Event{Type: agent.EventComplete})
	})
	rig := dialLoopback(t, script)

	var drawn *agent.SignInPrompt
	var order []agent.EventType
	for ev := range promptDefault(t, rig) {
		order = append(order, ev.Type)
		switch ev.Type {
		case agent.EventSignIn:
			drawn = ev.SignIn
			if ev.SignIn.Request != signInPrompt().Request {
				t.Fatalf("the sign-in crossed as %+v", ev.SignIn.Request)
			}
			ev.SignIn.Answer <- agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Code: "4/0Ab-code", UserID: "U9"}
		case agent.EventSignInSettled:
			if ev.SignInSettled.Prompt != drawn || ev.SignInSettled.State != agent.SignInSuccess {
				t.Fatalf("the sign-in settled as %+v for %p, drawn %p", ev.SignInSettled, ev.SignInSettled.Prompt, drawn)
			}
		case agent.EventError:
			t.Fatalf("the turn failed: %v", ev.Error)
		}
	}
	if got := awaitAnswer(t, codes, "the code"); got.Outcome != agent.DisplayAnswered || got.Code != "4/0Ab-code" {
		t.Fatalf("the node received %+v", got)
	}
	want := []agent.EventType{agent.EventSignIn, agent.EventSignInSettled, agent.EventText, agent.EventComplete}
	if len(order) != len(want) {
		t.Fatalf("the gateway saw %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("the gateway saw %v, want %v", order, want)
		}
	}
}

// A sign-in still open when its turn ends is cancelled on the node even after
// a code went through, so no sign-in process outlives the turn that started it.
func TestASignInOpenWhenItsTurnEndsIsCancelledOnTheNode(t *testing.T) {
	answers := make(chan agent.DisplayAnswer, 2)
	script := newScriptedAgent(func(turn *scriptedTurn) {
		go func() {
			prompt := signInPrompt()
			if !turnEmitter(turn)(agent.Event{Type: agent.EventSignIn, SignIn: prompt}) {
				return
			}
			for range 2 {
				select {
				case got := <-prompt.Answer:
					answers <- got
				case <-time.After(10 * time.Second):
					return
				}
			}
		}()
		<-turn.cancelled
	})
	rig := dialLoopback(t, script)

	manager := rig.sessions["default"]
	events := promptDefault(t, rig)
	for ev := range events {
		if ev.Type == agent.EventSignIn {
			ev.SignIn.Answer <- agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Code: "4/0Ab-code"}
			break
		}
	}
	if got := awaitAnswer(t, answers, "the code"); got.Code != "4/0Ab-code" {
		t.Fatalf("the node received %+v first", got)
	}
	sessionID, _ := manager.Lookup(agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"})
	cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = manager.Cancel(cancelCtx, sessionID)
	for range events {
	}

	if got := awaitAnswer(t, answers, "the cancel"); got.Outcome != agent.DisplayDismissed {
		t.Fatalf("a sign-in open when its turn ended was answered with %+v", got)
	}
}

// Nobody consumes a headless turn's cards, so the gateway refuses a sign-in on
// one instead of leaving the node's sign-in process waiting for its timeout.
func TestAHeadlessSignInIsRefusedByTheGateway(t *testing.T) {
	answered := make(chan agent.DisplayAnswer, 1)
	script := newScriptedAgent(func(turn *scriptedTurn) {
		prompt := signInPrompt()
		turnEmitter(turn)(agent.Event{Type: agent.EventSignIn, SignIn: prompt})
		select {
		case got := <-prompt.Answer:
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
			t.Fatalf("a headless sign-in was answered with %+v", got)
		}
	default:
		t.Fatal("a sign-in on a headless turn was never answered")
	}
}

// Outside a turn a sign-in has no stream to be answered on, so the node refuses
// it before anything is sent.
func TestABackgroundSignInIsRefusedOnTheNode(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}))

	prompt := signInPrompt()
	rig.background.Handle("node-session-1", agent.Event{Type: agent.EventSignIn, SignIn: prompt})

	if got := awaitAnswer(t, prompt.Answer, "the refusal"); got.Outcome != agent.DisplayNoConversation {
		t.Fatalf("a background sign-in was answered with %+v", got)
	}
	select {
	case got := <-rig.notices:
		t.Fatalf("a background sign-in reached the gateway's router as %+v", got.event)
	case <-time.After(200 * time.Millisecond):
	}
}

// A node that sends a sign-in outside any turn anyway is refused by the gateway.
func TestABackgroundSignInIsNotDrawnByTheGateway(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}))
	node := attachRaw(t, rig, "node-raw")

	node.send(t, mustBackground(t, "node-session-1", agentwire.Event{Type: agentwire.EventSignIn, SignIn: &agentwire.SignInRequest{
		ID: "sign-in-1", Tool: "gcp-mcp", Profile: "gcloud", URL: "https://accounts.example.com",
	}}))

	select {
	case got := <-node.answers:
		if got.ID != "sign-in-1" || got.Outcome != string(agent.DisplayNoConversation) {
			t.Fatalf("the gateway answered %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway never answered a background sign-in")
	}
	select {
	case got := <-rig.notices:
		t.Fatalf("a background sign-in reached the gateway's router as %+v", got.event)
	case <-time.After(200 * time.Millisecond):
	}
}

// Approving a command does not end a sign-in: the link and the code still have
// to cross afterwards.
func TestAnApprovedSignInStaysOpenForItsLinkAndCode(t *testing.T) {
	codes := make(chan agent.DisplayAnswer, 2)
	script := newScriptedAgent(func(turn *scriptedTurn) {
		prompt := &agent.SignInPrompt{
			Request: agent.SignInRequest{Tool: "vendor-mcp", Profile: "custom", Command: "vendor-cli login", NeedsCode: true},
			Answer:  make(chan agent.DisplayAnswer, 2),
		}
		if !turnEmitter(turn)(agent.Event{Type: agent.EventSignIn, SignIn: prompt}) {
			return
		}
		for range 2 {
			select {
			case got := <-prompt.Answer:
				codes <- got
				if got.Outcome == agent.DisplayApproved {
					turnEmitter(turn)(agent.Event{Type: agent.EventSignInSettled, SignInSettled: &agent.SignInSettled{Prompt: prompt, State: agent.SignInReady, URL: "https://vendor.example.com/device"}})
				}
			case <-time.After(10 * time.Second):
				return
			}
		}
		turnEmitter(turn)(agent.Event{Type: agent.EventSignInSettled, SignInSettled: &agent.SignInSettled{Prompt: prompt, State: agent.SignInSuccess}})
		turn.emit(agent.Event{Type: agent.EventComplete})
	})
	rig := dialLoopback(t, script)

	var link string
	for ev := range promptDefault(t, rig) {
		switch {
		case ev.SignIn != nil:
			if ev.SignIn.Request.Command != "vendor-cli login" || ev.SignIn.Request.URL != "" {
				t.Fatalf("the sign-in crossed as %+v", ev.SignIn.Request)
			}
			ev.SignIn.Answer <- agent.DisplayAnswer{Outcome: agent.DisplayApproved}
		case ev.SignInSettled != nil && ev.SignInSettled.State == agent.SignInReady:
			link = ev.SignInSettled.URL
			ev.SignInSettled.Prompt.Answer <- agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Code: "123-456"}
		}
	}
	if link != "https://vendor.example.com/device" {
		t.Fatalf("the link reached the gateway as %q", link)
	}
	if got := awaitAnswer(t, codes, "the approval"); got.Outcome != agent.DisplayApproved {
		t.Fatalf("the node received %+v first", got)
	}
	if got := awaitAnswer(t, codes, "the code"); got.Code != "123-456" {
		t.Fatalf("the code after an approval reached the node as %+v", got)
	}
}
