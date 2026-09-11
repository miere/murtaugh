// Package plan holds the `present_plan` tool's contract and none of its drawing,
// so the same tool runs on a node that cannot reach Slack.
package plan

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/agent"
)

// Display may ignore loc: on a node the gateway binds the plan to the
// conversation it came from, never to where the node says.
type Display interface {
	Plan(ctx context.Context, loc agent.TurnLocation, req agent.PlanRequest) (agent.DisplayAnswer, error)
}

// Tool is the `present_plan` capability.
type Tool struct {
	display Display
}

func New(display Display) *Tool { return &Tool{display: display} }

// Name returns the registry key.
func (t *Tool) Name() string { return "present_plan" }

// Description is the model-facing summary. It is deliberately explicit that the
// tool blocks for a real decision and that the agent must not proceed on its own.
func (t *Tool) Description() string {
	return "Present a plan to the user as a Slack message with Proceed / Revise / Cancel " +
		"buttons and WAIT for their decision before doing multi-step work. Use this to get " +
		"sign-off on a plan you intend to execute — never start until the user picks Proceed, " +
		"and never treat silence as approval. Returns whether they approved, plus a note on " +
		"what to do next. Only works inside a Slack conversation."
}

// InputSchema declares the arguments: the plan itself, plus an optional heading.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"plan": {
				Type:        "string",
				Description: "The plan to present, as multi-line markdown the user can review.",
			},
			"title": {Type: "string", Description: "Optional short heading shown above the plan."},
		},
		Required: []string{"plan"},
	}
}

// Result is the structured outcome. The MCP frontend JSON-marshals it; the loop
// and CLI render it via String().
type Result struct {
	Approved bool   `json:"approved"`
	Choice   string `json:"choice,omitempty"`
	Note     string `json:"note,omitempty"`
}

// String renders the line fed back to the model / shown in the CLI.
func (r Result) String() string {
	if r.Note != "" {
		return r.Note
	}
	if r.Approved {
		return "The user approved the plan."
	}
	return "The user did not approve the plan."
}

func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	if t.display == nil {
		return nil, errUnavailable()
	}
	loc, ok := agent.TurnLocationFromContext(ctx)
	if !ok {
		return nil, errNoConversation()
	}
	planText := strings.TrimSpace(stringArg(args, "plan"))
	if planText == "" {
		return nil, fmt.Errorf("Error: a plan is required")
	}
	title := strings.TrimSpace(stringArg(args, "title"))
	if title == "" {
		title = ":clipboard: Plan — approve?"
	}

	answer, err := t.display.Plan(ctx, loc, agent.PlanRequest{Title: title, Plan: planText})
	if err != nil {
		return nil, err
	}
	switch answer.Outcome {
	case agent.DisplayAnswered:
	case agent.DisplayTimedOut:
		return Result{Approved: false, Note: "No response in time. Do not assume approval — ask again or stop."}, nil
	case agent.DisplayDismissed:
		return Result{Approved: false, Note: "The plan prompt was dismissed before they answered."}, nil
	case agent.DisplayNoConversation:
		return nil, errNoConversation()
	default:
		if answer.Note != "" {
			return nil, errors.New(answer.Note)
		}
		return nil, errUnavailable()
	}
	switch answer.Choice {
	case agent.PlanProceed:
		return Result{Approved: true, Choice: "Proceed", Note: "Approved — proceed with the plan as presented."}, nil
	case agent.PlanRevise:
		return Result{Approved: false, Choice: "Revise", Note: "The user wants changes before you proceed. Ask what to adjust; do not start yet."}, nil
	case agent.PlanCancel:
		return Result{Approved: false, Choice: "Cancel", Note: "The user cancelled. Do not proceed."}, nil
	default:
		return Result{Approved: false, Choice: answer.Choice}, nil
	}
}

func errUnavailable() error {
	return fmt.Errorf("Error: interactive plan approval is not available in this context")
}

func errNoConversation() error {
	return fmt.Errorf("Error: the present_plan tool only works inside a Slack conversation")
}

func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}
