package ask

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	slackgo "github.com/slack-go/slack"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/slack/askcard"
	slacklib "github.com/miere/murtaugh/internal/slack/client"
	"github.com/miere/murtaugh/internal/slack/client/slacktest"
	"github.com/miere/murtaugh/internal/slack/interaction"
)

// newCardFlow builds an ask card flow over the signalling fake, so a test can
// watch the card go out and then click it.
func newCardFlow(sig *signalingAPI) *askcard.Flow {
	return askcard.New(
		slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return sig, nil }),
		askcard.NewRenderer("", assets.FS),
	)
}

type signalingAPI struct {
	*slacktest.FakeAPI
	posted chan slacklib.PostMessageParams
}

func (s *signalingAPI) PostMessage(ctx context.Context, p slacklib.PostMessageParams) (slacklib.PostMessageResult, error) {
	res, err := s.FakeAPI.PostMessage(ctx, p)
	s.posted <- p
	return res, err
}

func locatedCtx() context.Context {
	return agent.WithTurnLocation(context.Background(), agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"})
}

func TestInvoke_NilBrokerErrors(t *testing.T) {
	_, err := New(nil, nil).Invoke(locatedCtx(), map[string]any{"question": "q", "options": []any{"a", "b"}})
	if err == nil {
		t.Fatal("expected an error when the broker is unwired")
	}
}

func TestInvoke_RequiresSlackLocation(t *testing.T) {
	broker := interaction.NewWith((&slacktest.FakeAPI{}).LazyClient())
	_, err := New(broker, nil).Invoke(context.Background(), map[string]any{"question": "q", "options": []any{"a", "b"}})
	if err == nil || !strings.Contains(err.Error(), "Slack conversation") {
		t.Fatalf("expected a Slack-conversation error, got %v", err)
	}
}

func TestInvoke_RequiresTwoOptions(t *testing.T) {
	broker := interaction.NewWith((&slacktest.FakeAPI{}).LazyClient())
	_, err := New(broker, nil).Invoke(locatedCtx(), map[string]any{"question": "q", "options": []any{"only one"}})
	if err == nil {
		t.Fatal("expected an error for fewer than two options")
	}
}

func TestInvoke_PostsToTurnLocationAndReturnsChoice(t *testing.T) {
	sig := &signalingAPI{
		FakeAPI: &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1700.1"}},
		posted:  make(chan slacklib.PostMessageParams, 1),
	}
	broker := interaction.NewWith(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return sig, nil }))
	tool := New(broker, nil)

	resultCh := make(chan Result, 1)
	go func() {
		out, err := tool.Invoke(locatedCtx(), map[string]any{"question": "Ship it?", "options": []any{"Approve", "Deny"}})
		if err != nil {
			t.Errorf("Invoke error: %v", err)
			resultCh <- Result{}
			return
		}
		resultCh <- out.(Result)
	}()

	posted := <-sig.posted
	// The question is asked in the turn's own thread, not somewhere the model guessed.
	if posted.ChannelID != "C1" || posted.ThreadTS != "t1" {
		t.Fatalf("posted to %q/%q, want C1/t1", posted.ChannelID, posted.ThreadTS)
	}
	corr := corrFrom(t, posted.Blocks)
	if !broker.Resolve(corr, interaction.Decision{OptionID: "Approve", Label: "Approve", UserID: "U1"}) {
		t.Fatal("Resolve found no pending ask")
	}

	got := <-resultCh
	if !got.Answered || got.Choice != "Approve" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestInvoke_DismissedOnCancel(t *testing.T) {
	sig := &signalingAPI{
		FakeAPI: &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1700.1"}},
		posted:  make(chan slacklib.PostMessageParams, 1),
	}
	broker := interaction.NewWith(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return sig, nil }))
	ctx, cancel := context.WithCancel(locatedCtx())

	resultCh := make(chan Result, 1)
	go func() {
		out, _ := New(broker, nil).Invoke(ctx, map[string]any{"question": "q", "options": []any{"a", "b"}})
		resultCh <- out.(Result)
	}()

	<-sig.posted
	cancel()
	got := <-resultCh
	if got.Answered {
		t.Fatalf("expected an unanswered result on cancel, got %+v", got)
	}
}

func TestInvoke_MultiQuestionRoutesToCard(t *testing.T) {
	sig := &signalingAPI{
		FakeAPI: &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1700.1"}},
		posted:  make(chan slacklib.PostMessageParams, 1),
	}
	broker := interaction.NewWith(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return sig, nil }))
	flow := newCardFlow(sig)
	tool := New(broker, flow)

	resultCh := make(chan Result, 1)
	go func() {
		out, err := tool.Invoke(locatedCtx(), map[string]any{
			"title": "Deploy",
			"questions": []any{
				map[string]any{"label": "Env?", "options": []any{"Staging", "Production"}},
				map[string]any{"label": "Regions?", "multiSelect": true, "options": []any{"US", "EU"}},
			},
		})
		if err != nil {
			t.Errorf("Invoke error: %v", err)
			resultCh <- Result{}
			return
		}
		resultCh <- out.(Result)
	}()

	posted := <-sig.posted
	if posted.ChannelID != "C1" || posted.ThreadTS != "t1" {
		t.Fatalf("posted to %q/%q, want C1/t1", posted.ChannelID, posted.ThreadTS)
	}
	corr := cardCorrFrom(t, posted.Blocks)

	// Keys are assigned positionally (q0, q1) by the tool.
	answers := map[string][]string{"q0": {"Production"}, "q1": {"US", "EU"}}
	if err := flow.HandleClick(context.Background(), corr, askcard.ActionSubmit, "U1", answers); err != nil {
		t.Fatalf("HandleClick: %v", err)
	}

	got := <-resultCh
	if !got.Answered || len(got.Answers) != 2 {
		t.Fatalf("unexpected result: %+v", got)
	}
	if got.Answers[0].Question != "Env?" || len(got.Answers[0].Choices) != 1 || got.Answers[0].Choices[0] != "Production" {
		t.Fatalf("env answer wrong: %+v", got.Answers[0])
	}
	if len(got.Answers[1].Choices) != 2 {
		t.Fatalf("regions answer wrong: %+v", got.Answers[1])
	}
	if got.UserID != "U1" {
		t.Errorf("UserID = %q, want U1 — the model should be able to attribute the decision", got.UserID)
	}
}

// "Chat About This" is the escape hatch. It must NOT read to the model as a
// refusal or a timeout: the user is declining the offered options and asking to
// talk, so the note is phrased as their question back and restates what was
// asked.
func TestInvoke_ChatAboutThisAsksTheModelToDiscuss(t *testing.T) {
	sig := &signalingAPI{
		FakeAPI: &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1700.1"}},
		posted:  make(chan slacklib.PostMessageParams, 1),
	}
	broker := interaction.NewWith(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return sig, nil }))
	flow := newCardFlow(sig)

	resultCh := make(chan Result, 1)
	go func() {
		out, err := New(broker, flow).Invoke(locatedCtx(), map[string]any{
			"questions": []any{
				map[string]any{"label": "Env?", "options": []any{"Staging", "Production"}},
				map[string]any{"label": "Regions?", "multiSelect": true, "options": []any{"US", "EU"}},
			},
		})
		if err != nil {
			t.Errorf("Invoke error: %v", err)
			resultCh <- Result{}
			return
		}
		resultCh <- out.(Result)
	}()

	posted := <-sig.posted
	corr := cardCorrFrom(t, posted.Blocks)
	if err := flow.HandleClick(context.Background(), corr, askcard.ActionChat, "U1", nil); err != nil {
		t.Fatalf("HandleClick: %v", err)
	}

	got := <-resultCh
	if got.Answered {
		t.Fatalf("a chat request must not report as answered: %+v", got)
	}
	note := got.String()
	if !strings.Contains(note, "Can we chat about this?") {
		t.Errorf("note should carry the user's question verbatim; got %q", note)
	}
	// It restates the questions so the model can open the discussion without
	// digging back through its own transcript.
	for _, want := range []string{"Env?", "Regions?"} {
		if !strings.Contains(note, want) {
			t.Errorf("note should restate %q; got %q", want, note)
		}
	}
	// Nothing in it should suggest the user refused or went silent.
	for _, banned := range []string{"did not respond", "dismissed", "denied"} {
		if strings.Contains(note, banned) {
			t.Errorf("note reads as a refusal (%q); got %q", banned, note)
		}
	}
}

func TestInvoke_SinglePlainQuestionUsesButtonPath(t *testing.T) {
	sig := &signalingAPI{
		FakeAPI: &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1700.1"}},
		posted:  make(chan slacklib.PostMessageParams, 1),
	}
	broker := interaction.NewWith(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return sig, nil }))

	resultCh := make(chan Result, 1)
	go func() {
		// One plain single-select question expressed via `questions` should still
		// ride the simpler button path (no modal).
		out, err := New(broker, nil).Invoke(locatedCtx(), map[string]any{
			"questions": []any{map[string]any{"label": "Ship?", "options": []any{"Yes", "No"}}},
		})
		if err != nil {
			t.Errorf("Invoke error: %v", err)
			resultCh <- Result{}
			return
		}
		resultCh <- out.(Result)
	}()

	posted := <-sig.posted
	// It is a button prompt (interaction.go namespace), resolved by a click.
	corr := corrFrom(t, posted.Blocks)
	if !broker.Resolve(corr, interaction.Decision{OptionID: "Yes", Label: "Yes", UserID: "U1"}) {
		t.Fatal("Resolve found no pending ask")
	}
	got := <-resultCh
	if !got.Answered || got.Choice != "Yes" || len(got.Answers) != 0 {
		t.Fatalf("expected a button-path choice, got %+v", got)
	}
}

// cardCorrFrom recovers the correlation id from a posted ask card's Submit
// button (the murtaugh_ask: namespace). The card is raw JSON rather than typed
// slack-go blocks, so it is walked as a generic document.
func cardCorrFrom(t *testing.T, raw []byte) string {
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
		t.Fatalf("card blocks not valid JSON: %v", err)
	}
	for _, b := range doc.Blocks {
		for _, child := range b.ChildBlocks {
			if child.Type != "actions" {
				continue
			}
			for _, el := range child.Elements {
				if corr, _, ok := askcard.ParseActionID(el.ActionID); ok {
					return corr
				}
			}
		}
	}
	t.Fatal("no ask card buttons in posted blocks")
	return ""
}

func corrFrom(t *testing.T, raw []byte) string {
	t.Helper()
	var blocks slackgo.Blocks
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("blocks not valid JSON: %v", err)
	}
	for _, b := range blocks.BlockSet {
		if action, ok := b.(*slackgo.ActionBlock); ok && action.Elements != nil {
			for _, el := range action.Elements.ElementSet {
				if btn, ok := el.(*slackgo.ButtonBlockElement); ok {
					// action_id == "murtaugh_interaction:<corr>:<idx>"
					parts := strings.Split(btn.ActionID, ":")
					if len(parts) >= 3 {
						return parts[1]
					}
				}
			}
		}
	}
	t.Fatal("no broker button in posted blocks")
	return ""
}
