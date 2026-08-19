package interaction

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/slack/approvalcard"
)

func testCards() *approvalcard.Renderer { return approvalcard.NewRenderer("", assets.FS) }

// compact strips the templates' indentation so a structural assertion can be
// written as the JSON it is looking for rather than as a whitespace puzzle.
func compact(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact blocks: %v", err)
	}
	return buf.String()
}

// cardCorr walks a rendered card's container for the first button's action_id and
// pulls the correlation id out of it — which is exactly what the gateway router
// has to do on a click, so a card whose action_id cannot be found here is a card
// whose buttons are dead.
func cardCorr(t *testing.T, raw []byte) string {
	t.Helper()
	var doc struct {
		Blocks []struct {
			ChildBlocks []struct {
				Type     string `json:"type"`
				Elements []struct {
					ActionID string `json:"action_id"`
				} `json:"elements"`
			} `json:"child_blocks"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("posted card is not valid JSON: %v", err)
	}
	for _, b := range doc.Blocks {
		for _, cb := range b.ChildBlocks {
			if cb.Type != "actions" {
				continue
			}
			for _, el := range cb.Elements {
				if el.ActionID != "" {
					return correlationFromActionID(el.ActionID)
				}
			}
		}
	}
	t.Fatalf("no button action_id found in the posted card:\n%s", raw)
	return ""
}

// The gate must post the container card, and a click on a button nested inside
// its child_blocks must still be correlatable back to the blocked call.
func TestGateApprover_PostsCardAndCorrelates(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	broker.outcomeTTL = 0
	ctx := agent.WithTurnLocation(context.Background(), agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1", UserID: "U1"})

	done := make(chan struct{})
	go func() {
		NewApprover(broker, testCards(), false).Approve(ctx, "terminal", "rm -rf x")
		close(done)
	}()

	posted := <-sig.posted
	if !strings.Contains(compact(t, posted.Blocks), `"type":"container"`) {
		t.Fatalf("gate did not post the container card:\n%s", posted.Blocks)
	}
	// The fallback names the tool, never the command.
	if !strings.Contains(posted.Text, "terminal") || strings.Contains(posted.Text, "rm -rf") {
		t.Fatalf("notification text = %q, want the tool named and the command withheld", posted.Text)
	}
	if !broker.Resolve(cardCorr(t, posted.Blocks), Decision{OptionID: "approve", Label: "Approve", UserID: "U1"}) {
		t.Fatal("the correlation id recovered from the card resolved nothing")
	}
	<-done

	if len(sig.Updated) != 1 {
		t.Fatalf("expected one chat.update, got %d", len(sig.Updated))
	}
	if !strings.Contains(compact(t, sig.Updated[0].Blocks), `"type":"container"`) {
		t.Fatalf("settled state is not the container card:\n%s", sig.Updated[0].Blocks)
	}
	if !strings.Contains(string(sig.Updated[0].Blocks), "U1") {
		t.Fatalf("settled card does not name the decider:\n%s", sig.Updated[0].Blocks)
	}
}

// The command being approved has to reach the card, or the user is approving
// something they cannot see.
func TestGateApprover_CardCarriesTheCommand(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	broker.outcomeTTL = 0
	ctx := agent.WithTurnLocation(context.Background(), agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"})

	done := make(chan struct{})
	go func() {
		NewApprover(broker, testCards(), false).Approve(ctx, "terminal", "kubectl delete ns prod")
		close(done)
	}()
	posted := <-sig.posted
	if !strings.Contains(string(posted.Blocks), "kubectl delete ns prod") {
		t.Fatalf("card omits the command being approved:\n%s", posted.Blocks)
	}
	if !strings.Contains(compact(t, posted.Blocks), `"language":"bash"`) {
		t.Fatalf("a shell command should be highlighted as bash:\n%s", posted.Blocks)
	}
	broker.Resolve(cardCorr(t, posted.Blocks), Decision{OptionID: "deny", Label: "Deny", UserID: "U1"})
	<-done
}

// keep_resolved is the whole point of the flag: with it set, the settled card
// stays in the conversation as a record of who allowed what.
func TestGateApprover_KeepResolvedSuppressesDelete(t *testing.T) {
	for _, tc := range []struct {
		name         string
		keepResolved bool
		wantDeleted  bool
	}{
		{"default sweeps the settled card", false, true},
		{"keep_resolved leaves it in place", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			broker, sig := newSignalingBroker(t)
			// Short enough that the delete, if scheduled, lands inside the poll below.
			broker.outcomeTTL = time.Millisecond
			ctx := agent.WithTurnLocation(context.Background(), agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"})

			done := make(chan struct{})
			go func() {
				NewApprover(broker, testCards(), tc.keepResolved).Approve(ctx, "terminal", "ls")
				close(done)
			}()
			posted := <-sig.posted
			broker.Resolve(cardCorr(t, posted.Blocks), Decision{OptionID: "approve", Label: "Approve", UserID: "U1"})
			<-done

			deleted := waitForDelete(sig, 500*time.Millisecond)
			if deleted != tc.wantDeleted {
				t.Fatalf("card deleted = %v, want %v", deleted, tc.wantDeleted)
			}
			// Either way the settled state was written.
			if len(sig.Updated) != 1 {
				t.Fatalf("expected the settled card written once, got %d", len(sig.Updated))
			}
		})
	}
}

// waitForDelete polls for a chat.delete until the deadline. A negative result
// costs the full wait — which is the price of proving an absence.
func waitForDelete(sig *signalingAPI, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if len(sig.Deleted) > 0 {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return len(sig.Deleted) > 0
}

// An ACP agent's permission request renders through the same card, with whatever
// options the agent declared.
func TestPermissionGate_PostsCard(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	broker.outcomeTTL = 0
	gate := NewPermissionGate(broker, testCards(), false)

	done := make(chan struct{})
	go func() {
		gate.AskPermission(context.Background(),
			agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"},
			agent.PermissionRequest{
				ToolKind:  "execute",
				ToolTitle: "git push --force",
				Options: []agent.PermissionOption{
					{ID: "allow_once", Name: "Allow once", Kind: "allow_once"},
					{ID: "allow_always", Name: "Allow always", Kind: "allow_always"},
					{ID: "reject_once", Name: "Reject", Kind: "reject_once"},
				},
			})
		close(done)
	}()

	posted := <-sig.posted
	if !strings.Contains(compact(t, posted.Blocks), `"type":"container"`) {
		t.Fatalf("permission gate did not post the container card:\n%s", posted.Blocks)
	}
	if !strings.Contains(string(posted.Blocks), "git push --force") {
		t.Fatalf("card omits the command:\n%s", posted.Blocks)
	}
	if n := strings.Count(compact(t, posted.Blocks), `"action_id"`); n != 3 {
		t.Fatalf("rendered %d buttons, want the agent's 3", n)
	}
	broker.Resolve(cardCorr(t, posted.Blocks), Decision{OptionID: "reject_once", Label: "Reject", UserID: "U1"})
	<-done

	if !strings.Contains(string(sig.Updated[0].Blocks), "Denied") {
		t.Fatalf("a reject_* choice should settle as denied:\n%s", sig.Updated[0].Blocks)
	}
}

func TestNativeOutcome(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    Decision
		want approvalcard.Outcome
	}{
		{"approve", Decision{OptionID: "approve"}, approvalcard.OutcomeApproved},
		{"approve_always reads as approved", Decision{OptionID: "approve_always"}, approvalcard.OutcomeApproved},
		{"deny", Decision{OptionID: "deny"}, approvalcard.OutcomeDenied},
		{"timeout", Decision{TimedOut: true}, approvalcard.OutcomeTimedOut},
		{"cancelled", Decision{Cancelled: true}, approvalcard.OutcomeDismissed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nativeOutcome(tc.d); got != tc.want {
				t.Fatalf("nativeOutcome = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPermissionCardOutcome(t *testing.T) {
	kinds := map[string]string{
		"a": "allow_once",
		"b": "allow_always",
		"r": "reject_once",
		"x": "something_bespoke",
	}
	for _, tc := range []struct {
		name string
		d    Decision
		want approvalcard.Outcome
	}{
		{"allow_once", Decision{OptionID: "a"}, approvalcard.OutcomeApproved},
		{"allow_always", Decision{OptionID: "b"}, approvalcard.OutcomeApproved},
		{"reject_once", Decision{OptionID: "r"}, approvalcard.OutcomeDenied},
		// An agent-defined kind Murtaugh does not recognise: a choice was made, so
		// the card must not claim anybody refused.
		{"unknown kind is not a refusal", Decision{OptionID: "x"}, approvalcard.OutcomeApproved},
		{"timeout beats the kind", Decision{OptionID: "r", TimedOut: true}, approvalcard.OutcomeTimedOut},
		{"cancel beats the kind", Decision{OptionID: "r", Cancelled: true}, approvalcard.OutcomeDismissed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := permissionCardOutcome(kinds)(tc.d); got != tc.want {
				t.Fatalf("permissionCardOutcome = %q, want %q", got, tc.want)
			}
		})
	}
}

// A command line must be highlighted as shell whatever the backend calls its
// tool: Murtaugh's is "terminal", ACP's kind is "execute", and Claude Code's is
// literally "bash".
func TestCodeLangCoversEveryShellToolName(t *testing.T) {
	for _, name := range []string{"terminal", "bash", "Bash", "execute", "shell", " bash "} {
		if got := codeLang(name); got != "bash" {
			t.Errorf("codeLang(%q) = %q, want \"bash\"", name, got)
		}
	}
	for _, name := range []string{"edit", "read", "write", ""} {
		if got := codeLang(name); got != "" {
			t.Errorf("codeLang(%q) = %q, want no hint", name, got)
		}
	}
}

// With no renderer wired the gate falls back to the broker's plain button row, so
// an embedding that has no templates still works.
func TestGateApprover_NilCardsFallsBackToPlainPrompt(t *testing.T) {
	broker, sig := newSignalingBroker(t)
	broker.outcomeTTL = 0
	ctx := agent.WithTurnLocation(context.Background(), agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"})

	done := make(chan struct{})
	go func() {
		NewApprover(broker, nil, false).Approve(ctx, "terminal", "ls")
		close(done)
	}()
	posted := <-sig.posted
	if strings.Contains(compact(t, posted.Blocks), `"type":"container"`) {
		t.Fatalf("nil renderer should not produce a card:\n%s", posted.Blocks)
	}
	broker.Resolve(corrFromPosted(t, posted), Decision{OptionID: "approve", Label: "Approve", UserID: "U1"})
	<-done
}
