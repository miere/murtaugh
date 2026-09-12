package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/providerfail"
	"github.com/miere/murtaugh/internal/slack/alertcard"
)

func geminiOverload() error {
	return providerfail.New(providerfail.Failure{
		Kind:       providerfail.Overloaded,
		Provider:   "gemini",
		StatusCode: 503,
		Message:    "This model is currently experiencing high demand. Spikes in demand are usually temporary. Please try again later.",
		Retryable:  true,
	}, `native: provider stream: llm: gemini stream: provider error (HTTP 503): {"error":{"code":503,"message":"This model is currently experiencing high demand. Spikes in demand are usually temporary. Please try again later.","status":"UNAVAILABLE"}}`)
}

// TestFailSpecProviderFailure pins what the user actually reads when a native
// agent's provider is down — the incident this replaced was a three-layer Go
// error chain wrapped around a pretty-printed JSON body.
func TestFailSpecProviderFailure(t *testing.T) {
	spec := failSpec(context.Background(), geminiOverload(), nil)

	if spec.Level != alertcard.LevelError {
		t.Errorf("Level = %q, want error", spec.Level)
	}
	if spec.Subtitle != "The agent is not available." {
		t.Errorf("Subtitle = %q", spec.Subtitle)
	}
	if spec.Reason != "Gemini is overloaded (503)" {
		t.Errorf("Reason = %q, want the classified headline", spec.Reason)
	}
	if !strings.Contains(spec.Text, "experiencing high demand") {
		t.Errorf("Text = %q, want the provider's own sentence", spec.Text)
	}
	// The remedy follows from the kind: an overload is worth waiting out.
	if spec.NextSteps != "Try again in a moment." {
		t.Errorf("NextSteps = %q", spec.NextSteps)
	}
	if strings.Contains(spec.Subtitle, "ACP agent") {
		t.Error("provider failure must not be attributed to the ACP agent")
	}
}

// The collapsed card is what makes keeping the whole chain affordable: it costs
// no screen space until someone opens it, and it is exactly what diagnosing the
// failure needs.
func TestFailSpecKeepsTheUnabridgedError(t *testing.T) {
	err := geminiOverload()

	if got := failSpec(context.Background(), err, nil).Detail; got != err.Error() {
		t.Errorf("Detail = %q, want the full error chain %q", got, err.Error())
	}
}

// Everything that is not a provider error keeps the generic headline with the
// raw error, which is what diagnosing an ACP or spawn fault needs.
func TestFailSpecNonProviderFailure(t *testing.T) {
	spec := failSpec(context.Background(), errors.New("acp: session terminated"), nil)

	if spec.Subtitle != "Murtaugh hit an error while talking to the agent." {
		t.Errorf("Subtitle = %q, want the generic notice", spec.Subtitle)
	}
	if spec.Detail != "acp: session terminated" {
		t.Errorf("Detail = %q, want the raw error", spec.Detail)
	}
	if spec.Reason != "" {
		t.Errorf("Reason = %q, want none for an unclassified error", spec.Reason)
	}
	// No hand-written next steps, so the card supplies the level's own.
	if !strings.Contains(alertcard.PlainText(spec), "notify your admin user") {
		t.Errorf("rendered alert carries no guidance: %q", alertcard.PlainText(spec))
	}
}

// Fail(nil) still produces a usable alert rather than an empty card.
func TestFailSpecNilError(t *testing.T) {
	spec := failSpec(context.Background(), nil, nil)

	if spec.Level != alertcard.LevelError {
		t.Errorf("Level = %q, want error", spec.Level)
	}
	if spec.Subtitle == "" {
		t.Error("a nil error must still say something")
	}
	if spec.Detail != "" {
		t.Errorf("Detail = %q, want empty for a nil error", spec.Detail)
	}
	if got := alertcard.PlainText(spec); strings.HasSuffix(got, "\n") {
		t.Errorf("PlainText = %q, want no trailing newline", got)
	}
}

// Being full is a warning, not a failure: nothing broke, every slot is simply
// occupied. The card has to say so in the user's terms — how many are running
// and who to ask for more — without the error styling that sends people looking
// for a fault that does not exist.
func TestFailSpecAtCapacityWarnsRatherThanErrors(t *testing.T) {
	spec := failSpec(context.Background(), fmt.Errorf("prompt agent: %w", &agent.CapacityError{Limit: 12}), nil)

	if spec.Level != alertcard.LevelWarn {
		t.Errorf("Level = %q, want a warning — nothing is broken", spec.Level)
	}
	if spec.Title != "I'm really busy at the moment" {
		t.Errorf("Title = %q, want the busy headline", spec.Title)
	}
	if !strings.Contains(spec.Subtitle, "12 tasks running") {
		t.Errorf("Subtitle = %q, want the configured max_sessions in it", spec.Subtitle)
	}
	if !strings.Contains(spec.NextSteps, "admin user") {
		t.Errorf("NextSteps = %q, want the route to raising the limit", spec.NextSteps)
	}
	// The wrapping is still worth keeping for whoever opens the card.
	if !strings.Contains(spec.Detail, "all 12 session slots") {
		t.Errorf("Detail = %q, want the unabridged cause", spec.Detail)
	}
}

// The e2e gap behind spec #170: a turn with no machine to run on got the generic
// card, which said neither that the machine was off nor what to do about it.
func TestFailSpecNoMachineOffersAWayForward(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		reason string
	}{
		{
			name:   "nothing is connected",
			err:    fmt.Errorf("initialize agent client: %w", agentruntime.ErrNoNode),
			reason: "No machine is connected to Murtaugh right now.",
		},
		{
			name:   "nothing of the user's is connected",
			err:    fmt.Errorf("create agent session: %w", agentruntime.ErrNoFleet),
			reason: "None of your machines is connected, and you hold no grant on anyone else's.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := failSpec(context.Background(), tc.err, nil)

			if spec.Level != alertcard.LevelWarn {
				t.Errorf("Level = %q, want a warning — nothing is broken", spec.Level)
			}
			if spec.Title != "No machine available" {
				t.Errorf("Title = %q", spec.Title)
			}
			if spec.Subtitle != "No machine is available to run this conversation." {
				t.Errorf("Subtitle = %q", spec.Subtitle)
			}
			if spec.Reason != tc.reason {
				t.Errorf("Reason = %q, want %q", spec.Reason, tc.reason)
			}
			if spec.NextSteps != "Start your runtime node (`murtaugh-runtime`) and try again, or ask the gateway admin for a grant on theirs." {
				t.Errorf("NextSteps = %q", spec.NextSteps)
			}
			if spec.Detail != tc.err.Error() {
				t.Errorf("Detail = %q, want the unabridged cause", spec.Detail)
			}
		})
	}
}

func TestFailSpecNamesTheMachineAConversationWasOn(t *testing.T) {
	err := fmt.Errorf("initialize agent client: %w", &agentruntime.NodeOfflineError{
		Node: agentruntime.NodeRef{NodeID: "e2e-node", Owner: "U0ADMIN"},
		Err:  agentruntime.ErrNoNode,
	})

	spec := failSpec(context.Background(), err, nil)

	if spec.Title != "Machine offline" {
		t.Errorf("Title = %q", spec.Title)
	}
	if spec.Subtitle != "The machine this conversation was running on is offline." {
		t.Errorf("Subtitle = %q", spec.Subtitle)
	}
	want := "`e2e-node`, <@U0ADMIN>'s machine, is not connected, and no machine of yours can take the conversation over."
	if spec.Reason != want {
		t.Errorf("Reason = %q, want %q", spec.Reason, want)
	}
	if !strings.Contains(spec.NextSteps, "murtaugh-runtime") {
		t.Errorf("NextSteps = %q, want the way forward", spec.NextSteps)
	}
}

// A token may name an owner that is not a Slack user ID, which would render as a
// broken mention.
func TestFailSpecLeavesOutAnOwnerThatIsNotASlackUser(t *testing.T) {
	spec := failSpec(context.Background(), &agentruntime.NodeOfflineError{
		Node: agentruntime.NodeRef{NodeID: "e2e-node", Owner: "miere"},
		Err:  agentruntime.ErrNoFleet,
	}, nil)

	if want := "`e2e-node` is not connected, and no machine of yours can take the conversation over."; spec.Reason != want {
		t.Errorf("Reason = %q, want %q", spec.Reason, want)
	}
}

// Classified by identity, so an error that merely reads the same is still a fault.
func TestFailSpecDoesNotMatchTheSentinelsByText(t *testing.T) {
	spec := failSpec(context.Background(), errors.New("initialize agent client: "+agentruntime.ErrNoNode.Error()), nil)

	if spec.Subtitle != "Murtaugh hit an error while talking to the agent." {
		t.Errorf("Subtitle = %q, want the generic notice", spec.Subtitle)
	}
	if spec.Level != alertcard.LevelError {
		t.Errorf("Level = %q, want error", spec.Level)
	}
}
