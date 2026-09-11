package remote

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
)

func promptWithSignIn(t *testing.T, owner string) ([]agent.Event, string) {
	t.Helper()
	client, node, logs := dial(t, nodeConfig{}, Options{Owner: owner})
	events, err := client.Prompt(context.Background(), "session-42", agent.PromptRequest{Text: "sign in", Channel: "C1", Thread: "1.1", User: "U9"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	stream := receive(t, "the accepted stream", node.streams)
	node.emit(stream, agentwire.Event{Type: agentwire.EventSignIn, SignIn: &agentwire.SignInRequest{ID: "sign-in-1", Tool: "gcp-mcp", Profile: "gcloud", URL: "https://accounts.example.com"}})
	node.emit(stream, agentwire.Event{Type: agentwire.EventText, Text: "after"})
	node.end(stream)
	var got []agent.Event
	for ev := range events {
		got = append(got, ev)
	}
	return got, logs.String()
}

// A node's sign-in is only ever drawn for the owner its credential names.
func TestASignInIsStampedWithTheConnectionsOwner(t *testing.T) {
	events, _ := promptWithSignIn(t, "UOWNER")
	if len(events) == 0 || events[0].SignIn == nil || events[0].SignIn.Owner != "UOWNER" {
		t.Fatalf("the sign-in arrived as %+v", events)
	}
}

// A connection that names no owner has nobody to ask, and must not fall back to
// the gateway admin, who does not own the node's credentials.
func TestASignInFromANodeWithNoOwnerIsRefused(t *testing.T) {
	events, logs := promptWithSignIn(t, "")
	for _, ev := range events {
		if ev.SignIn != nil {
			t.Fatalf("a sign-in from a node with no owner was handed on to be drawn: %+v", ev.SignIn)
		}
	}
	if len(events) != 1 || events[0].Text != "after" {
		t.Fatalf("the turn went on as %+v", events)
	}
	if !strings.Contains(logs, "names no owner") {
		t.Fatalf("the refusal was not logged:\n%s", logs)
	}
}

func raiseWithNoTurn(t *testing.T, node *fakeNode, id string) agentwire.Message {
	t.Helper()
	return raise(t, node, id, "sign-in-"+id, "gcp-mcp")
}

func raise(t *testing.T, node *fakeNode, requestID, signInID, tool string) agentwire.Message {
	t.Helper()
	request, err := agentwire.Request(requestID, agentwire.MethodSignIn, agentwire.SignInRequest{ID: signInID, Tool: tool, Profile: "gcloud", URL: "https://accounts.example.com"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	node.send(request)
	return receive(t, "the gateway's answer", node.responses)
}

func showAndWait(ctx context.Context, _ *agent.SignInPrompt, _ <-chan agent.SignInSettled, shown func(error)) {
	shown(nil)
	<-ctx.Done()
}

func TestASignInWithNoTurnIsDrawnForTheOwner(t *testing.T) {
	drawn := make(chan *agent.SignInPrompt, 1)
	updates := make(chan agent.SignInSettled, 2)
	_, node, _ := dial(t, nodeConfig{}, Options{Owner: "UOWNER", SignIns: func(ctx context.Context, prompt *agent.SignInPrompt, settled <-chan agent.SignInSettled, shown func(error)) {
		drawn <- prompt
		shown(nil)
		prompt.Answer <- agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Code: "4/0Ab-code", UserID: "UOWNER"}
		for update := range settled {
			updates <- update
			if update.State.Terminal() {
				return
			}
		}
	}})

	if fault := raiseWithNoTurn(t, node, "1").Fault(); fault != nil {
		t.Fatalf("the gateway refused the sign-in: %v", fault)
	}
	prompt := receive(t, "the drawing", drawn)
	if prompt.Owner != "UOWNER" || prompt.Request.Tool != "gcp-mcp" {
		t.Fatalf("drew %+v for %q", prompt.Request, prompt.Owner)
	}
	if got := receive(t, "the code", node.answers); got.ID != "sign-in-1" || got.Code != "4/0Ab-code" {
		t.Fatalf("the node received %+v", got)
	}
	settle, err := agentwire.Request("2", agentwire.MethodSignInSettled, agentwire.SignInSettled{ID: "sign-in-1", State: string(agent.SignInSuccess)})
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	node.send(settle)
	if got := receive(t, "the settle", updates); got.Prompt != prompt || got.State != agent.SignInSuccess {
		t.Fatalf("the drawing heard %+v", got)
	}
	if fault := receive(t, "the settle's answer", node.responses).Fault(); fault != nil {
		t.Fatalf("the settle was refused: %v", fault)
	}
}

func TestASignInWithNoTurnFromANodeWithNoOwnerIsRefused(t *testing.T) {
	drawn := make(chan struct{}, 1)
	_, node, logs := dial(t, nodeConfig{}, Options{SignIns: func(context.Context, *agent.SignInPrompt, <-chan agent.SignInSettled, func(error)) {
		drawn <- struct{}{}
	}})
	fault := raiseWithNoTurn(t, node, "1").Fault()
	if fault == nil || !strings.Contains(fault.Error(), "names no owner") {
		t.Fatalf("a node with no owner was answered %v", fault)
	}
	select {
	case <-drawn:
		t.Fatal("a sign-in from a node with no owner was drawn")
	default:
	}
	if !strings.Contains(logs.String(), "names no owner") {
		t.Fatalf("the refusal was not logged:\n%s", logs.String())
	}
}

func TestASignInWithNoTurnIsAcceptedOnlyOnceItsCardIsShown(t *testing.T) {
	show := make(chan error)
	_, node, _ := dial(t, nodeConfig{}, Options{Owner: "UOWNER", SignIns: func(ctx context.Context, _ *agent.SignInPrompt, _ <-chan agent.SignInSettled, shown func(error)) {
		shown(<-show)
		<-ctx.Done()
	}})

	request, err := agentwire.Request("1", agentwire.MethodSignIn, agentwire.SignInRequest{ID: "sign-in-1", Tool: "gcp-mcp", Profile: "gcloud"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	node.send(request)
	select {
	case early := <-node.responses:
		t.Fatalf("the gateway answered %+v before the card was shown", early)
	case <-time.After(200 * time.Millisecond):
	}
	show <- nil
	if fault := receive(t, "the acceptance", node.responses).Fault(); fault != nil {
		t.Fatalf("a shown card was answered %v", fault)
	}

	request, _ = agentwire.Request("2", agentwire.MethodSignIn, agentwire.SignInRequest{ID: "sign-in-2", Tool: "vendor-mcp", Profile: "gcloud"})
	node.send(request)
	show <- errors.New("the owner of this machine may not use this gateway")
	if fault := receive(t, "the refusal", node.responses).Fault(); fault == nil || !strings.Contains(fault.Error(), "may not use this gateway") {
		t.Fatalf("a card that could not be shown was answered %v", fault)
	}
}

func TestASignInWithNoTurnIsRefusedPastItsCaps(t *testing.T) {
	_, node, _ := dial(t, nodeConfig{}, Options{Owner: "UOWNER", SignIns: showAndWait})

	if fault := raise(t, node, "1", "a", "tool-a").Fault(); fault != nil {
		t.Fatalf("the first sign-in was refused: %v", fault)
	}
	if fault := raise(t, node, "2", "a-again", "tool-a").Fault(); fault == nil {
		t.Fatal("a second sign-in for the same tool was drawn while the first was open")
	}
	for i, tool := range []string{"tool-b", "tool-c", "tool-d"} {
		if fault := raise(t, node, fmt.Sprint(3+i), tool, tool).Fault(); fault != nil {
			t.Fatalf("sign-in for %s was refused: %v", tool, fault)
		}
	}
	if fault := raise(t, node, "9", "e", "tool-e").Fault(); fault == nil {
		t.Fatal("a fifth sign-in was drawn while four were open")
	}
}

func TestTwoSignInsWithOneIDAreNeverBothDrawn(t *testing.T) {
	var drawn atomic.Int32
	client, node, _ := dial(t, nodeConfig{}, Options{Owner: "UOWNER", SignIns: func(ctx context.Context, _ *agent.SignInPrompt, settled <-chan agent.SignInSettled, shown func(error)) {
		drawn.Add(1)
		shown(nil)
		for update := range settled {
			if update.State.Terminal() {
				return
			}
		}
	}})
	for i := range 20 {
		id := fmt.Sprint("dup-", i)
		for j := range 2 {
			request, _ := agentwire.Request(fmt.Sprint(i, "-", j), agentwire.MethodSignIn, agentwire.SignInRequest{ID: id, Tool: fmt.Sprint("tool-", j), Profile: "gcloud"})
			node.send(request)
		}
		accepted := 0
		for range 2 {
			if receive(t, "an answer", node.responses).Fault() == nil {
				accepted++
			}
		}
		if accepted != 1 || drawn.Load() != 1 {
			t.Fatalf("round %d: %d of two sign-ins sharing one id were accepted and %d drawn", i, accepted, drawn.Load())
		}
		settle, _ := agentwire.Request(fmt.Sprint("s-", i), agentwire.MethodSignInSettled, agentwire.SignInSettled{ID: id, State: string(agent.SignInCancelled)})
		node.send(settle)
		receive(t, "the settle's answer", node.responses)
		deadline := time.Now().Add(3 * time.Second)
		for headlessOpen(client) > 0 {
			if time.Now().After(deadline) {
				t.Fatalf("round %d: the settled sign-in was never released", i)
			}
			time.Sleep(5 * time.Millisecond)
		}
		drawn.Store(0)
	}
}

func headlessOpen(c *Client) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.headless)
}

func TestAHeadlessCardTheNodeNeverSettlesIsWithdrawnAtTheGatewaysDeadline(t *testing.T) {
	defer func(d time.Duration) { headlessSignInDeadline = d }(headlessSignInDeadline)
	headlessSignInDeadline = 100 * time.Millisecond
	_, node, _ := dial(t, nodeConfig{}, Options{Owner: "UOWNER", SignIns: func(ctx context.Context, prompt *agent.SignInPrompt, _ <-chan agent.SignInSettled, shown func(error)) {
		shown(nil)
		<-ctx.Done()
		prompt.Answer <- agent.DisplayAnswer{Outcome: agent.DisplayDismissed}
	}})
	if fault := raiseWithNoTurn(t, node, "1").Fault(); fault != nil {
		t.Fatalf("the sign-in was refused: %v", fault)
	}
	if got := receive(t, "the withdrawal", node.answers); got.ID != "sign-in-1" || got.Outcome != string(agent.DisplayDismissed) {
		t.Fatalf("a card past its deadline answered the node with %+v", got)
	}
}
