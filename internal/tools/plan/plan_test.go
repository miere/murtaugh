package plan

import (
	"context"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
)

type fakeDisplay struct {
	answer agent.DisplayAnswer
	loc    agent.TurnLocation
	req    agent.PlanRequest
	calls  int
}

func (d *fakeDisplay) Plan(_ context.Context, loc agent.TurnLocation, req agent.PlanRequest) (agent.DisplayAnswer, error) {
	d.calls++
	d.loc, d.req = loc, req
	return d.answer, nil
}

func locatedCtx() context.Context {
	return agent.WithTurnLocation(context.Background(), agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"})
}

func TestInvoke_NilDisplayErrors(t *testing.T) {
	_, err := New(nil).Invoke(locatedCtx(), map[string]any{"plan": "do the thing"})
	if err == nil {
		t.Fatal("expected an error when nothing can draw the plan")
	}
}

// Refusing up front, before anything is drawn, is what keeps a headless run from
// raising a plan nobody can approve.
func TestInvoke_RequiresSlackLocation(t *testing.T) {
	d := &fakeDisplay{}
	_, err := New(d).Invoke(context.Background(), map[string]any{"plan": "do the thing"})
	if err == nil || err.Error() != "Error: the present_plan tool only works inside a Slack conversation" {
		t.Fatalf("expected the Slack-conversation refusal, got %v", err)
	}
	if d.calls != 0 {
		t.Fatal("a plan with no conversation reached the display")
	}
}

func TestInvoke_RequiresPlan(t *testing.T) {
	_, err := New(&fakeDisplay{}).Invoke(locatedCtx(), map[string]any{"plan": "   "})
	if err == nil {
		t.Fatal("expected an error for an empty plan")
	}
}

func TestInvoke_ProceedIsApproved(t *testing.T) {
	d := &fakeDisplay{answer: agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Choice: agent.PlanProceed}}
	out, err := New(d).Invoke(locatedCtx(), map[string]any{"plan": "1. step one\n2. step two"})
	if err != nil {
		t.Fatalf("Invoke error: %v", err)
	}
	if d.loc.ChannelID != "C1" || d.loc.ThreadTS != "t1" {
		t.Fatalf("the display was handed %+v, want the turn's own location", d.loc)
	}
	if d.req.Plan != "1. step one\n2. step two" || d.req.Title == "" {
		t.Fatalf("the display was asked %+v", d.req)
	}
	if got := out.(Result); !got.Approved || got.Choice != "Proceed" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestInvoke_OnlyProceedIsApproved(t *testing.T) {
	for _, answer := range []agent.DisplayAnswer{
		{Outcome: agent.DisplayAnswered, Choice: agent.PlanRevise},
		{Outcome: agent.DisplayAnswered, Choice: agent.PlanCancel},
		{Outcome: agent.DisplayTimedOut},
		{Outcome: agent.DisplayDismissed},
	} {
		out, err := New(&fakeDisplay{answer: answer}).Invoke(locatedCtx(), map[string]any{"plan": "ship it"})
		if err != nil {
			t.Fatalf("Invoke error: %v", err)
		}
		if got := out.(Result); got.Approved || got.Note == "" {
			t.Errorf("%+v came back as %+v; it must not approve and must say why", answer, got)
		}
	}
}

// A gateway that finds no conversation behind the turn answers no_conversation,
// and the model must read the same refusal it would have got up front.
func TestInvoke_GatewayRefusalReadsAsTheHeadlessRefusal(t *testing.T) {
	_, err := New(&fakeDisplay{answer: agent.DisplayAnswer{Outcome: agent.DisplayNoConversation}}).
		Invoke(locatedCtx(), map[string]any{"plan": "ship it"})
	if err == nil || err.Error() != "Error: the present_plan tool only works inside a Slack conversation" {
		t.Fatalf("expected the Slack-conversation refusal, got %v", err)
	}
}
