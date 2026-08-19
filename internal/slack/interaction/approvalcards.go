package interaction

import (
	"strings"

	"github.com/miere/murtaugh/internal/slack/approvalcard"
)

// approvalCards adapts an approvalcard.Renderer to the broker's CardRenderer for
// one gated tool call. It carries the two things the renderer needs and the
// prompt cannot supply: what the call is (the tool and its command), and how to
// read a Decision as an outcome — which differs between the native gate's fixed
// Approve/Deny options and an ACP agent's self-declared ones.
type approvalCards struct {
	cards   *approvalcard.Renderer
	spec    approvalcard.Spec
	outcome func(Decision) approvalcard.Outcome
}

// Pending forwards the broker's already-addressed buttons to the card. The
// action_id and value are copied through untouched: they are how a click is
// correlated back to the blocked call.
func (a approvalCards) Pending(_ PromptSpec, opts []CardOption) ([]byte, error) {
	buttons := make([]approvalcard.Option, 0, len(opts))
	for _, o := range opts {
		buttons = append(buttons, approvalcard.Option{
			ActionID: o.ActionID,
			Value:    o.Value,
			Label:    o.Label,
			Style:    o.Style,
		})
	}
	// BlockID is the broker's own routing constant, passed in so approvalcard
	// never has to know it.
	return a.cards.Pending(a.spec, BlockID, buttons)
}

func (a approvalCards) Resolved(_ PromptSpec, d Decision) ([]byte, error) {
	return a.cards.Resolved(a.spec, a.outcome(d), d.UserID)
}

func (a approvalCards) Fallback(PromptSpec) string { return approvalcard.FallbackText(a.spec) }

// nativeOutcome reads the native gate's fixed option set as a card outcome. Both
// approve and approve_always report as approved: what the settled card says is
// that the tool ran, and the always-allow set is a session detail the reader of
// the thread has no use for.
func nativeOutcome(d Decision) approvalcard.Outcome {
	switch {
	case d.TimedOut:
		return approvalcard.OutcomeTimedOut
	case d.Cancelled:
		return approvalcard.OutcomeDismissed
	case d.OptionID == "deny":
		return approvalcard.OutcomeDenied
	default:
		return approvalcard.OutcomeApproved
	}
}

// permissionOutcome reads an ACP agent's own permission options as a card
// outcome, keyed by the kinds the agent declared for them.
//
// An unrecognised kind reports as approved: the kinds are agent-defined, and the
// only thing Murtaugh knows for certain is that the user picked an option and
// the agent will act on it. Only an explicit reject_* is reported as a denial, so
// the card never claims somebody refused when nobody did.
func permissionCardOutcome(kindByID map[string]string) func(Decision) approvalcard.Outcome {
	return func(d Decision) approvalcard.Outcome {
		switch {
		case d.TimedOut:
			return approvalcard.OutcomeTimedOut
		case d.Cancelled:
			return approvalcard.OutcomeDismissed
		}
		if strings.HasPrefix(kindByID[d.OptionID], "reject") {
			return approvalcard.OutcomeDenied
		}
		return approvalcard.OutcomeApproved
	}
}
