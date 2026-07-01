package interaction

import (
	"context"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
)

// TestPermissionGate_Outcome verifies the ACP permission gate mirrors the native
// approval experience: the prompt is posted as a normal threaded message in the
// turn's thread, and the choice rewrites it (chat.update) to a concise line keyed
// on the option kind — allow shows a check, reject is struck through, both naming
// the decider.
func TestPermissionGate_Outcome(t *testing.T) {
	cases := []struct {
		name     string
		optionID string
		want     string
	}{
		{"allow", "allow_once", "✓ Tool `terminal` approved by <@U1>"},
		{"reject", "reject_once", "~Tool `terminal` denied by <@U1>~"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker, sig := newSignalingBroker(t)
			broker.outcomeTTL = 0 // assert on the single outcome write; skip the async delete
			gate := NewPermissionGate(broker)
			loc := agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1", UserID: "U1"}
			req := agent.PermissionRequest{
				ToolKind:  "execute", // surfaced to the human as "terminal"
				ToolTitle: "ls -la",
				Options: []agent.PermissionOption{
					{ID: "allow_once", Name: "Allow", Kind: "allow_once"},
					{ID: "reject_once", Name: "Deny", Kind: "reject_once"},
				},
			}

			done := make(chan struct{})
			go func() {
				gate.AskPermission(context.Background(), loc, req)
				close(done)
			}()

			posted := <-sig.posted
			if len(sig.Posted) != 1 || posted.ThreadTS != "t1" {
				t.Fatalf("permission prompt should be a single message in the turn's thread, got %+v", sig.Posted)
			}
			// The prompt names the tool concisely (execute → terminal) and renders
			// the command in a bash-hinted fenced code block rather than inline.
			if !strings.Contains(string(posted.Blocks), "`terminal`") {
				t.Fatalf("prompt should name the tool concisely, got %s", posted.Blocks)
			}
			if !strings.Contains(string(posted.Blocks), "```bash") || !strings.Contains(string(posted.Blocks), "ls -la") {
				t.Fatalf("prompt should render the command in a bash code block, got %s", posted.Blocks)
			}
			broker.Resolve(corrFromPosted(t, posted), Decision{OptionID: tc.optionID, Label: "x", UserID: "U1"})
			<-done

			if len(sig.Updated) != 1 {
				t.Fatalf("expected the outcome written once via chat.update, got %d", len(sig.Updated))
			}
			if got := sig.Updated[0].Text; got != tc.want {
				t.Fatalf("outcome text = %q, want %q", got, tc.want)
			}
		})
	}
}
