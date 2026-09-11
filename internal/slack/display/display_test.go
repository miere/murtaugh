package display

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

type signalingAPI struct {
	*slacktest.FakeAPI
	posted chan slacklib.PostMessageParams
}

func (s *signalingAPI) PostMessage(ctx context.Context, p slacklib.PostMessageParams) (slacklib.PostMessageResult, error) {
	res, err := s.FakeAPI.PostMessage(ctx, p)
	s.posted <- p
	return res, err
}

type rig struct {
	sig    *signalingAPI
	broker *interaction.Broker
	cards  *askcard.Flow
	slack  *Slack
}

func newRig() *rig {
	sig := &signalingAPI{
		FakeAPI: &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1700.1"}},
		posted:  make(chan slacklib.PostMessageParams, 1),
	}
	client := slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return sig, nil })
	broker := interaction.NewWith(client)
	cards := askcard.New(client, askcard.NewRenderer("", assets.FS))
	return &rig{sig: sig, broker: broker, cards: cards, slack: New(broker, cards)}
}

var here = agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"}

type outcome struct {
	answer agent.DisplayAnswer
	err    error
}

func TestAnUnwiredDisplayErrors(t *testing.T) {
	if _, err := New(nil, nil).Question(context.Background(), here, agent.QuestionRequest{}); err == nil {
		t.Fatal("expected an error when the broker is unwired")
	}
	if _, err := New(nil, nil).Plan(context.Background(), here, agent.PlanRequest{Plan: "x"}); err == nil {
		t.Fatal("expected an error when the broker is unwired")
	}
}

func TestAPlainQuestionIsAButtonPromptInTheGivenThread(t *testing.T) {
	r := newRig()
	done := make(chan outcome, 1)
	go func() {
		a, err := r.slack.Question(context.Background(), here, agent.QuestionRequest{Questions: []agent.Question{{
			Key: "q0", Question: "Ship it?", Options: []agent.QuestionOption{{Label: "Approve"}, {Label: "Deny"}},
		}}})
		done <- outcome{a, err}
	}()

	posted := <-r.sig.posted
	if posted.ChannelID != "C1" || posted.ThreadTS != "t1" {
		t.Fatalf("posted to %q/%q, want C1/t1", posted.ChannelID, posted.ThreadTS)
	}
	if !r.broker.Resolve(buttonCorr(t, posted.Blocks), interaction.Decision{OptionID: "Approve", Label: "Approve", UserID: "U1"}) {
		t.Fatal("Resolve found no pending ask")
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("Question error: %v", got.err)
	}
	if got.answer.Outcome != agent.DisplayAnswered || len(got.answer.Answers["q0"]) != 1 || got.answer.Answers["q0"][0] != "Approve" {
		t.Fatalf("unexpected answer: %+v", got.answer)
	}
}

func TestACancelledQuestionIsDismissed(t *testing.T) {
	r := newRig()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan outcome, 1)
	go func() {
		a, err := r.slack.Question(ctx, here, agent.QuestionRequest{Questions: []agent.Question{{
			Key: "q0", Question: "q", Options: []agent.QuestionOption{{Label: "a"}, {Label: "b"}},
		}}})
		done <- outcome{a, err}
	}()
	<-r.sig.posted
	cancel()
	if got := <-done; got.answer.Outcome != agent.DisplayDismissed {
		t.Fatalf("expected a dismissed answer on cancel, got %+v (%v)", got.answer, got.err)
	}
}

func TestSeveralQuestionsAreOneCard(t *testing.T) {
	r := newRig()
	done := make(chan outcome, 1)
	go func() {
		a, err := r.slack.Question(context.Background(), here, agent.QuestionRequest{Title: "Deploy", Questions: []agent.Question{
			{Key: "q0", Question: "Env?", Options: []agent.QuestionOption{{Label: "Staging"}, {Label: "Production"}}},
			{Key: "q1", Question: "Regions?", MultiSelect: true, Options: []agent.QuestionOption{{Label: "US"}, {Label: "EU"}}},
		}})
		done <- outcome{a, err}
	}()

	posted := <-r.sig.posted
	if posted.ChannelID != "C1" || posted.ThreadTS != "t1" {
		t.Fatalf("posted to %q/%q, want C1/t1", posted.ChannelID, posted.ThreadTS)
	}
	answers := map[string][]string{"q0": {"Production"}, "q1": {"US", "EU"}}
	if err := r.cards.HandleClick(context.Background(), cardCorr(t, posted.Blocks), askcard.ActionSubmit, "U1", answers); err != nil {
		t.Fatalf("HandleClick: %v", err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("Question error: %v", got.err)
	}
	if got.answer.Outcome != agent.DisplayAnswered || len(got.answer.Answers["q1"]) != 2 || got.answer.UserID != "U1" {
		t.Fatalf("unexpected answer: %+v", got.answer)
	}
}

func TestChatAboutThisIsItsOwnOutcome(t *testing.T) {
	r := newRig()
	done := make(chan outcome, 1)
	go func() {
		a, err := r.slack.Question(context.Background(), here, agent.QuestionRequest{Questions: []agent.Question{
			{Key: "q0", Question: "Env?", Options: []agent.QuestionOption{{Label: "Staging"}, {Label: "Production"}}},
			{Key: "q1", Question: "Regions?", MultiSelect: true, Options: []agent.QuestionOption{{Label: "US"}, {Label: "EU"}}},
		}})
		done <- outcome{a, err}
	}()
	posted := <-r.sig.posted
	if err := r.cards.HandleClick(context.Background(), cardCorr(t, posted.Blocks), askcard.ActionChat, "U1", nil); err != nil {
		t.Fatalf("HandleClick: %v", err)
	}
	if got := <-done; got.answer.Outcome != agent.DisplayChat {
		t.Fatalf("expected a chat outcome, got %+v (%v)", got.answer, got.err)
	}
}

func TestAPlanOffersProceedReviseCancelInTheGivenThread(t *testing.T) {
	for _, choice := range []string{agent.PlanProceed, agent.PlanCancel} {
		r := newRig()
		done := make(chan outcome, 1)
		go func() {
			a, err := r.slack.Plan(context.Background(), here, agent.PlanRequest{Title: "Plan", Plan: "1. step one"})
			done <- outcome{a, err}
		}()

		posted := <-r.sig.posted
		if posted.ChannelID != "C1" || posted.ThreadTS != "t1" {
			t.Fatalf("posted to %q/%q, want C1/t1", posted.ChannelID, posted.ThreadTS)
		}
		if !r.broker.Resolve(buttonCorr(t, posted.Blocks), interaction.Decision{OptionID: choice, Label: choice, UserID: "U1"}) {
			t.Fatal("Resolve found no pending prompt")
		}
		got := <-done
		if got.err != nil || got.answer.Outcome != agent.DisplayAnswered || got.answer.Choice != choice {
			t.Fatalf("clicking %s came back as %+v (%v)", choice, got.answer, got.err)
		}
	}
}

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

func buttonCorr(t *testing.T, raw []byte) string {
	t.Helper()
	var blocks slackgo.Blocks
	if err := json.Unmarshal(raw, &blocks); err != nil {
		t.Fatalf("blocks not valid JSON: %v", err)
	}
	for _, b := range blocks.BlockSet {
		if action, ok := b.(*slackgo.ActionBlock); ok && action.Elements != nil {
			for _, el := range action.Elements.ElementSet {
				if btn, ok := el.(*slackgo.ButtonBlockElement); ok {
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
