package display

import (
	"context"
	"fmt"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/slack/askcard"
	"github.com/miere/murtaugh/internal/slack/interaction"
)

// Slack draws questions and plans for agents in this process and for agents on
// nodes alike, so a card looks the same wherever the tool ran.
type Slack struct {
	broker *interaction.Broker
	cards  *askcard.Flow
}

func New(broker *interaction.Broker, cards *askcard.Flow) *Slack {
	return &Slack{broker: broker, cards: cards}
}

func (s *Slack) Question(ctx context.Context, loc agent.TurnLocation, req agent.QuestionRequest) (agent.DisplayAnswer, error) {
	if s == nil || s.broker == nil {
		return agent.DisplayAnswer{}, fmt.Errorf("Error: interactive questions are not available in this context")
	}
	if req.Plain() {
		return s.buttons(ctx, loc, req)
	}
	if s.cards == nil {
		return agent.DisplayAnswer{}, fmt.Errorf("Error: interactive questions are not available in this context")
	}
	resp, err := s.cards.Ask(ctx, askcard.Destination{ChannelID: loc.ChannelID, ThreadTS: loc.ThreadTS}, askcard.Spec{
		Title:     req.Title,
		Questions: cardQuestions(req.Questions),
	})
	if err != nil {
		return agent.DisplayAnswer{}, err
	}
	switch {
	case resp.TimedOut:
		return agent.DisplayAnswer{Outcome: agent.DisplayTimedOut}, nil
	case resp.Cancelled:
		return agent.DisplayAnswer{Outcome: agent.DisplayDismissed}, nil
	case resp.Chat:
		return agent.DisplayAnswer{Outcome: agent.DisplayChat, UserID: resp.UserID}, nil
	}
	return agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Answers: resp.Answers, UserID: resp.UserID}, nil
}

func (s *Slack) buttons(ctx context.Context, loc agent.TurnLocation, req agent.QuestionRequest) (agent.DisplayAnswer, error) {
	q := req.Questions[0]
	options := make([]interaction.Option, 0, len(q.Options))
	for _, o := range q.Options {
		options = append(options, interaction.Option{ID: o.Label, Label: o.Label})
	}
	decision, err := s.broker.Ask(ctx, interaction.Destination{ChannelID: loc.ChannelID, ThreadTS: loc.ThreadTS}, interaction.PromptSpec{
		Title:    req.Title,
		Question: q.Question,
		Options:  options,
	})
	if err != nil {
		return agent.DisplayAnswer{}, err
	}
	if outcome, done := unanswered(decision); done {
		return agent.DisplayAnswer{Outcome: outcome}, nil
	}
	return agent.DisplayAnswer{
		Outcome: agent.DisplayAnswered,
		Answers: map[string][]string{q.Key: {decision.Label}},
		UserID:  decision.UserID,
	}, nil
}

func (s *Slack) Plan(ctx context.Context, loc agent.TurnLocation, req agent.PlanRequest) (agent.DisplayAnswer, error) {
	if s == nil || s.broker == nil {
		return agent.DisplayAnswer{}, fmt.Errorf("Error: interactive plan approval is not available in this context")
	}
	decision, err := s.broker.Ask(ctx, interaction.Destination{ChannelID: loc.ChannelID, ThreadTS: loc.ThreadTS}, interaction.PromptSpec{
		Title:    req.Title,
		Question: req.Plan,
		Options: []interaction.Option{
			{ID: agent.PlanProceed, Label: "Proceed", Style: "primary"},
			{ID: agent.PlanRevise, Label: "Revise"},
			{ID: agent.PlanCancel, Label: "Cancel", Style: "danger"},
		},
	})
	if err != nil {
		return agent.DisplayAnswer{}, err
	}
	if outcome, done := unanswered(decision); done {
		return agent.DisplayAnswer{Outcome: outcome}, nil
	}
	return agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Choice: decision.OptionID, UserID: decision.UserID}, nil
}

func unanswered(d interaction.Decision) (agent.DisplayOutcome, bool) {
	switch {
	case d.TimedOut:
		return agent.DisplayTimedOut, true
	case d.Cancelled:
		return agent.DisplayDismissed, true
	}
	return "", false
}

func cardQuestions(questions []agent.Question) []askcard.Question {
	out := make([]askcard.Question, 0, len(questions))
	for _, q := range questions {
		opts := make([]askcard.Option, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, askcard.Option{Label: o.Label, Description: o.Description})
		}
		out = append(out, askcard.Question{
			Key:         q.Key,
			Header:      q.Header,
			Question:    q.Question,
			Options:     opts,
			MultiSelect: q.MultiSelect,
		})
	}
	return out
}
