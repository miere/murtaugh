package interaction

import (
	"context"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
)

// policyRequest is what a backend with no options of its own sends: the tool and
// its command, and nothing about which buttons to show.
func policyRequest(tool, title string) agent.PermissionRequest {
	return agent.PermissionRequest{ToolKind: tool, ToolTitle: title, PolicyOwned: true}
}

// A delegating backend gets Murtaugh's own set — including the always-allow
// button that a hardcoded allow/deny pair in the backend could never grow.
func TestPolicyOwnedOffersMurtaughsOptions(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	broker.outcomeTTL = 0
	gate := NewPermissionGate(broker, testCards(), false, NewGrants())

	done := make(chan struct{})
	go func() {
		gate.AskPermission(context.Background(),
			agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"},
			policyRequest("Bash", "git push --force"))
		close(done)
	}()

	posted := <-sig.posted
	blocks := compact(t, posted.Blocks)
	if n := strings.Count(blocks, `"action_id"`); n != 3 {
		t.Fatalf("rendered %d buttons, want Murtaugh's 3", n)
	}
	for _, want := range []string{"Approve", "Approve & always allow", "Deny"} {
		if !strings.Contains(blocks, want) {
			t.Errorf("card is missing the %q button", want)
		}
	}
	// It is still a proper card, with the command shown and highlighted.
	if !strings.Contains(blocks, `"type":"container"`) || !strings.Contains(blocks, `"language":"bash"`) {
		t.Errorf("delegated call did not render the shell card:\n%s", posted.Blocks)
	}
	broker.Resolve(cardCorr(t, posted.Blocks), Decision{OptionID: policyOptionApprove, Label: "Approve", UserID: "U1"})
	<-done
}

// The backend's vocabulary stays allow / deny / nobody-chose. Murtaugh's own
// option ids must never leak across that boundary.
func TestPolicyOwnedAnswersInTheBackendsVocabulary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision Decision
		want     string
	}{
		{"approve", Decision{OptionID: policyOptionApprove, UserID: "U1"}, agent.PermissionAllow},
		{"always allow answers as a plain allow", Decision{OptionID: policyOptionAlways, UserID: "U1"}, agent.PermissionAllow},
		{"deny", Decision{OptionID: policyOptionDeny, UserID: "U1"}, agent.PermissionDeny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broker, sig := newSignalingBroker(t)
			broker.outcomeTTL = 0
			gate := NewPermissionGate(broker, testCards(), false, NewGrants())

			var got string
			done := make(chan struct{})
			go func() {
				got, _ = gate.AskPermission(context.Background(),
					agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"},
					policyRequest("Bash", "ls"))
				close(done)
			}()
			posted := <-sig.posted
			broker.Resolve(cardCorr(t, posted.Blocks), tc.decision)
			<-done

			if got != tc.want {
				t.Fatalf("AskPermission = %q, want %q", got, tc.want)
			}
		})
	}
}

// A timeout or dismissal is not a refusal, and the backend distinguishes them —
// they must come back as "" rather than as a deny.
func TestPolicyOwnedUnansweredIsNotADenial(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision Decision
	}{
		{"timeout", Decision{TimedOut: true}},
		{"dismissed", Decision{Cancelled: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broker, sig := newSignalingBroker(t)
			broker.outcomeTTL = 0
			gate := NewPermissionGate(broker, testCards(), false, NewGrants())

			var got string
			done := make(chan struct{})
			go func() {
				got, _ = gate.AskPermission(context.Background(),
					agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"},
					policyRequest("Bash", "ls"))
				close(done)
			}()
			posted := <-sig.posted
			broker.Resolve(cardCorr(t, posted.Blocks), tc.decision)
			<-done

			if got != "" {
				t.Fatalf("AskPermission = %q, want \"\" so the backend can tell this from a refusal", got)
			}
		})
	}
}

// The whole point of item 2: "always allow" on a delegated card creates a grant,
// and the next identical call is answered without posting anything.
func TestPolicyOwnedAlwaysAllowCreatesAGrant(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	broker.outcomeTTL = 0
	grants := NewGrants()
	gate := NewPermissionGate(broker, testCards(), false, grants)

	done := make(chan struct{})
	go func() {
		gate.AskPermission(context.Background(),
			agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"},
			policyRequest("Bash", "git diff"))
		close(done)
	}()
	posted := <-sig.posted
	broker.Resolve(cardCorr(t, posted.Blocks), Decision{OptionID: policyOptionAlways, Label: "Approve & always allow", UserID: "U1"})
	<-done
	postsAfterGrant := len(sig.Posted)

	got, err := gate.AskPermission(context.Background(),
		agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"},
		policyRequest("Bash", "git diff"))
	if err != nil {
		t.Fatalf("AskPermission: %v", err)
	}
	if got != agent.PermissionAllow {
		t.Fatalf("second call = %q, want it answered from the grant", got)
	}
	if len(sig.Posted) != postsAfterGrant {
		t.Fatalf("the granted call posted a card (%d → %d)", postsAfterGrant, len(sig.Posted))
	}
}

// A grant made on a delegated card must also hold for Murtaugh's own gate — that
// is the shared set doing its job in the direction item 2 makes possible.
func TestPolicyGrantAlsoSuppressesMurtaughsOwnGate(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	broker.outcomeTTL = 0
	grants := NewGrants()
	gate := NewPermissionGate(broker, testCards(), false, grants)

	done := make(chan struct{})
	go func() {
		gate.AskPermission(context.Background(),
			agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"},
			policyRequest("Bash", "git diff"))
		close(done)
	}()
	posted := <-sig.posted
	broker.Resolve(cardCorr(t, posted.Blocks), Decision{OptionID: policyOptionAlways, UserID: "U1"})
	<-done
	postsAfterGrant := len(sig.Posted)

	ctx := agent.WithTurnLocation(context.Background(), agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"})
	allowed, note := NewApprover(broker, testCards(), false, grants).Approve(ctx, "terminal", "git diff")
	if !allowed || note != "" {
		t.Fatalf("Murtaugh's gate re-asked a call granted on the agent's card: allowed=%v note=%q", allowed, note)
	}
	if len(sig.Posted) != postsAfterGrant {
		t.Fatalf("Murtaugh's gate posted a card for a granted call (%d → %d)", postsAfterGrant, len(sig.Posted))
	}
}

// A delegated denial must read as a denial on the settled card, not as an
// approval — the kinds Murtaugh attaches to its own options drive that.
func TestPolicyOwnedDenialRendersAsDenied(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	broker.outcomeTTL = 0
	gate := NewPermissionGate(broker, testCards(), false, NewGrants())

	done := make(chan struct{})
	go func() {
		gate.AskPermission(context.Background(),
			agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"},
			policyRequest("Bash", "rm -rf /"))
		close(done)
	}()
	posted := <-sig.posted
	broker.Resolve(cardCorr(t, posted.Blocks), Decision{OptionID: policyOptionDeny, Label: "Deny", UserID: "U1"})
	<-done

	if !strings.Contains(string(sig.Updated[0].Blocks), "Denied") {
		t.Fatalf("settled card does not report the denial:\n%s", sig.Updated[0].Blocks)
	}
}

// PolicyOwned must not disturb a real ACP agent: its options still come through
// untouched, and Murtaugh adds none of its own.
func TestReflectedOptionsUnaffectedByPolicyPath(t *testing.T) {
	offered := []agent.PermissionOption{
		{ID: "a", Name: "Allow once", Kind: "allow_once"},
		{ID: "b", Name: "Reject", Kind: "reject_once"},
	}
	options, kinds := reflectedOptions(offered)
	if len(options) != 2 || options[0].ID != "a" || options[1].ID != "b" {
		t.Fatalf("reflected options changed: %+v", options)
	}
	if kinds["a"] != "allow_once" || kinds["b"] != "reject_once" {
		t.Fatalf("reflected kinds changed: %+v", kinds)
	}
	// An option with no name falls back to its id rather than rendering blank.
	if got, _ := reflectedOptions([]agent.PermissionOption{{ID: "bare"}}); got[0].Label != "bare" {
		t.Fatalf("unnamed option rendered as %q", got[0].Label)
	}
}
