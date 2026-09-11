package display

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/slack/askcard"
	"github.com/miere/murtaugh/internal/slack/authcard"
	"github.com/miere/murtaugh/internal/slack/interaction"
)

// Slack draws questions and plans for agents in this process and for agents on
// nodes alike, so a card looks the same wherever the tool ran.
type Slack struct {
	broker  *interaction.Broker
	cards   *askcard.Flow
	signIns *authcard.Flow
	log     *slog.Logger
}

func New(broker *interaction.Broker, cards *askcard.Flow) *Slack {
	return &Slack{broker: broker, cards: cards}
}

func (s *Slack) WithSignIns(flow *authcard.Flow, log *slog.Logger) *Slack {
	if log == nil {
		log = slog.Default()
	}
	s.signIns, s.log = flow, log
	return s
}

// SignIn answers the prompt when ctx ends, because the process running the
// sign-in stops only when it is told to.
func (s *Slack) SignIn(ctx context.Context, loc agent.TurnLocation, prompt *agent.SignInPrompt, settled <-chan agent.SignInSettled) {
	s.ShowSignIn(ctx, loc, prompt, settled, nil)
}

// ShowSignIn reports whether the card was drawn before any answer can arrive,
// so a node never claims its owner was asked when nobody was.
func (s *Slack) ShowSignIn(ctx context.Context, loc agent.TurnLocation, prompt *agent.SignInPrompt, settled <-chan agent.SignInSettled, shown func(error)) {
	if shown == nil {
		shown = func(error) {}
	}
	answer := func(a agent.DisplayAnswer) {
		select {
		case prompt.Answer <- a:
		default:
		}
	}
	if s == nil || s.signIns == nil {
		shown(errors.New("sign-ins are not available in this context"))
		answer(agent.DisplayAnswer{Outcome: agent.DisplayUnavailable, Note: "Error: sign-ins are not available in this context"})
		return
	}
	req := prompt.Request
	card, err := s.signIns.Show(ctx, authcard.Showing{
		ToolName:        req.Tool,
		ProfileName:     req.Profile,
		URL:             req.URL,
		NeedsCode:       req.NeedsCode,
		Requester:       authcard.Destination{ChannelID: loc.ChannelID, ThreadTS: loc.ThreadTS},
		RequesterUserID: loc.UserID,
		Recipient:       prompt.Owner,
		Command:         req.Command,
	})
	if err != nil {
		s.log.Warn("could not show a sign-in", "tool", req.Tool, "owner", prompt.Owner, "error", err)
		note := "the sign-in could not be shown in Slack"
		if errors.Is(err, authcard.ErrNotAllowed) {
			note = "the owner of this machine may not use this gateway, so nobody was asked to sign in"
		}
		shown(errors.New(note))
		answer(agent.DisplayAnswer{Outcome: agent.DisplayUnavailable, Note: note})
		return
	}
	shown(nil)
	for {
		select {
		case reply := <-card.Replies():
			switch reply.Kind {
			case authcard.ReplyApproved:
				answer(agent.DisplayAnswer{Outcome: agent.DisplayApproved, UserID: reply.UserID})
				continue
			case authcard.ReplyCode:
				answer(agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Code: reply.Code, UserID: reply.UserID})
				continue
			case authcard.ReplyDenied:
				answer(agent.DisplayAnswer{Outcome: agent.DisplayDenied, UserID: reply.UserID})
				card.Settle(authcard.StateDenied, "")
			default:
				answer(agent.DisplayAnswer{Outcome: agent.DisplayUnavailable, Note: authcard.RefusedReason})
				card.Settle(authcard.StateFailed, authcard.RefusedReason)
			}
			return
		case update := <-settled:
			switch {
			case update.State == agent.SignInReady:
				card.Link(ctx, update.URL)
			case !update.State.Terminal():
				card.Working(ctx)
			default:
				card.Settle(cardState(update.State), update.Reason)
				return
			}
		case <-ctx.Done():
			answer(agent.DisplayAnswer{Outcome: agent.DisplayDismissed})
			card.Settle(authcard.StateCancelled, "")
			return
		}
	}
}

func cardState(s agent.SignInState) authcard.State {
	switch s {
	case agent.SignInSuccess:
		return authcard.StateSuccess
	case agent.SignInTimedOut:
		return authcard.StateTimeout
	case agent.SignInCancelled:
		return authcard.StateCancelled
	}
	return authcard.StateFailed
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
