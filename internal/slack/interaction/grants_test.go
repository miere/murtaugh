package interaction

import (
	"context"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
)

// The point of the shared set: a grant made through Murtaugh's own gate keys the
// same as the identical command arriving from an agent's own harness, where the
// tool is called something else entirely.
func TestGrantKeyCrossesThePaths(t *testing.T) {
	if a, b := GrantKey("terminal", "git diff"), GrantKey("bash", "git diff"); a != b {
		t.Fatalf("terminal key %q != bash key %q; a grant would not cross paths", a, b)
	}
	// "execute" is the ACP spelling of the same thing.
	if a, b := GrantKey("terminal", "git diff"), GrantKey("execute", "git diff"); a != b {
		t.Fatalf("terminal key %q != execute key %q", a, b)
	}
}

// Non-shell tools keep the tool name in the key, so allowing an edit says nothing
// about a read of the same path.
func TestGrantKeySeparatesNonShellTools(t *testing.T) {
	if GrantKey("edit", "src/main.go") == GrantKey("read", "src/main.go") {
		t.Fatal("edit and read of the same path share a key")
	}
	if GrantKey("edit", "src/main.go") == GrantKey("bash", "src/main.go") {
		t.Fatal("a non-shell tool collided with a shell command")
	}
}

// An empty detail must not produce a key: it would match every other detail-less
// call and silently allow them all.
func TestGrantKeyEmptyDetailIsNotGrantable(t *testing.T) {
	if k := GrantKey("bash", "   "); k != "" {
		t.Fatalf("GrantKey with blank detail = %q, want empty", k)
	}
	g := NewGrants()
	g.Remember(GrantKey("bash", ""))
	if g.Allowed("") {
		t.Fatal("an empty key was granted")
	}
}

func TestGrantsNilIsSafeAndDeniesEverything(t *testing.T) {
	var g *Grants
	g.Remember("anything") // must not panic
	if g.Allowed("anything") {
		t.Fatal("a nil grant set allowed a call")
	}
}

// End-to-end for item 3: the user allows a command once through Murtaugh's own
// gate, and the agent's later request for the same command is answered from the
// grant without posting anything.
func TestGrantFromApproverSuppressesTheAgentsRequest(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	broker.outcomeTTL = 0
	grants := NewGrants()
	ctx := agent.WithTurnLocation(context.Background(), agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"})

	// 1. Murtaugh's own terminal tool: the user picks "Approve & always allow".
	done := make(chan struct{})
	go func() {
		NewApprover(broker, testCards(), false, grants).Approve(ctx, "terminal", "git diff")
		close(done)
	}()
	posted := <-sig.posted
	broker.Resolve(cardCorr(t, posted.Blocks), Decision{OptionID: "approve_always", Label: "Approve & always allow", UserID: "U1"})
	<-done
	postsAfterGrant := len(sig.Posted)

	// 2. The agent now asks to run the same command through its own harness.
	gate := NewPermissionGate(broker, testCards(), false, grants)
	id, err := gate.AskPermission(context.Background(),
		agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"},
		agent.PermissionRequest{
			ToolKind:  "Bash",
			ToolTitle: "git diff",
			Options: []agent.PermissionOption{
				{ID: "allow", Name: "Allow", Kind: "allow_once"},
				{ID: "deny", Name: "Deny", Kind: "reject_once"},
			},
		})
	if err != nil {
		t.Fatalf("AskPermission: %v", err)
	}
	if id != "allow" {
		t.Fatalf("pre-granted call answered with %q, want the agent's allow option", id)
	}
	if len(sig.Posted) != postsAfterGrant {
		t.Fatalf("a pre-granted call posted a card (%d → %d)", postsAfterGrant, len(sig.Posted))
	}
}

// A command that was never granted must still be asked about.
func TestUngrantedCallStillAsks(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	broker.outcomeTTL = 0
	gate := NewPermissionGate(broker, testCards(), false, NewGrants())

	done := make(chan struct{})
	go func() {
		gate.AskPermission(context.Background(),
			agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"},
			agent.PermissionRequest{
				ToolKind:  "Bash",
				ToolTitle: "rm -rf /",
				Options:   []agent.PermissionOption{{ID: "allow", Name: "Allow", Kind: "allow_once"}},
			})
		close(done)
	}()
	posted := <-sig.posted
	broker.Resolve(cardCorr(t, posted.Blocks), Decision{OptionID: "allow", Label: "Allow", UserID: "U1"})
	<-done
}

// Item 1: the reflection path renders exactly the options the agent declared and
// adds nothing. An agent offering allow_always keeps its own button — Murtaugh
// must not shadow it with one of its own.
func TestPermissionGateAddsNoOptionsOfItsOwn(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	broker.outcomeTTL = 0
	gate := NewPermissionGate(broker, testCards(), false, NewGrants())

	opts := []agent.PermissionOption{
		{ID: "allow_once", Name: "Allow once", Kind: "allow_once"},
		{ID: "allow_always", Name: "Allow always", Kind: "allow_always"},
		{ID: "reject_once", Name: "Reject", Kind: "reject_once"},
	}
	done := make(chan struct{})
	go func() {
		gate.AskPermission(context.Background(),
			agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"},
			agent.PermissionRequest{ToolKind: "execute", ToolTitle: "ls", Options: opts})
		close(done)
	}()
	posted := <-sig.posted
	blocks := compact(t, posted.Blocks)
	if n := strings.Count(blocks, `"action_id"`); n != len(opts) {
		t.Fatalf("rendered %d buttons for %d agent options — Murtaugh added or dropped one", n, len(opts))
	}
	for _, o := range opts {
		if !strings.Contains(blocks, o.Name) {
			t.Errorf("agent option %q is missing from the card", o.Name)
		}
	}
	broker.Resolve(cardCorr(t, posted.Blocks), Decision{OptionID: "allow_once", Label: "Allow once", UserID: "U1"})
	<-done
}

// Answering a pre-granted call must prefer allow_once: a grant Murtaugh is
// holding should not be escalated into a standing permission on the agent's side.
func TestAllowOptionIDPrefersAllowOnce(t *testing.T) {
	got := allowOptionID([]agent.PermissionOption{
		{ID: "always", Kind: "allow_always"},
		{ID: "once", Kind: "allow_once"},
		{ID: "no", Kind: "reject_once"},
	})
	if got != "once" {
		t.Fatalf("allowOptionID = %q, want the allow_once option", got)
	}
}

// With no allow option at all there is nothing safe to answer with, so the call
// must fall through to the human rather than be invented.
func TestAllowOptionIDWithoutAnAllowOption(t *testing.T) {
	if got := allowOptionID([]agent.PermissionOption{{ID: "no", Kind: "reject_once"}}); got != "" {
		t.Fatalf("allowOptionID = %q, want empty", got)
	}
}

// A grant is scoped to one agent. Two agents get separate sets, so a permissive
// agent cannot widen a strict one.
func TestGrantsAreNotSharedBetweenAgents(t *testing.T) {
	a, b := NewGrants(), NewGrants()
	a.Remember(GrantKey("bash", "git push"))
	if b.Allowed(GrantKey("bash", "git push")) {
		t.Fatal("a grant leaked into another agent's set")
	}
}
