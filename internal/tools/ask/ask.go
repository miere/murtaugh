// Package ask implements the `ask` tool: the agent's way to put a question with
// a few options in front of the user as clickable Slack buttons and WAIT for the
// answer, instead of assuming one. It is the model-driven consumer of the shared
// interaction broker (internal/slack/interaction).
//
// It only works inside a Slack conversation: the turn's location is read from the
// context the native client stashes per turn, so the question is asked in the
// same thread the agent is talking in — not the admin DM, and not wherever the
// model guesses. Outside a chat turn (CLI/MCP) it returns an error rather than
// blocking.
package ask

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/slack/askcard"
	"github.com/miere/murtaugh/internal/slack/interaction"
)

// Tool is the `ask` capability.
//
// It has two transports. A single quick choice rides the interaction broker's
// button prompt; anything richer — several questions, or a multi-select — goes
// to the askcard Flow, which posts one card carrying every question as an inline
// input. Both are inert when nil, which is the right behaviour in CLI/MCP
// processes that have no gateway to route a click back.
type Tool struct {
	broker *interaction.Broker
	cards  *askcard.Flow
}

// New constructs an ask Tool against the shared interaction broker and ask card
// flow.
func New(broker *interaction.Broker, cards *askcard.Flow) *Tool {
	return &Tool{broker: broker, cards: cards}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "ask" }

// MCPName publishes this tool to LLM clients as AskUserQuestion rather than
// `ask`.
//
// Claude Code ships a built-in of that name which cannot render in a headless
// session — the claudecode backend suppresses it with --disallowedTools. A model
// that has learned to reach for AskUserQuestion then finds one, with the payload
// it already knows, and never has to be told the substitution happened. The
// registry key stays `ask`, so `murtaugh ask` and the CLI are untouched.
func (t *Tool) MCPName() string { return "AskUserQuestion" }

// Description is the model-facing summary. It is deliberately explicit that the
// tool blocks for a real answer and must not be second-guessed.
func (t *Tool) Description() string {
	return "Ask the user one or more questions and WAIT for their answer. Use this whenever " +
		"you need a decision, confirmation, or input before acting — never assume the answer " +
		"or treat silence as approval. The questions are posted to the user's chat as a card " +
		"of multiple-choice inputs they fill in and submit. Returns what they chose. The user " +
		"may instead ask to discuss the questions, in which case you get their question back " +
		"and should talk it through before deciding. Only works inside a chat conversation."
}

// InputSchema is Claude's AskUserQuestion payload, field for field.
//
// That is the whole point rather than a coincidence: the claudecode backend hides
// Claude Code's built-in of the same name (it cannot render headlessly) and this
// tool stands in for it. A model reaching for the tool it already knows must find
// the arguments it already knows, or the substitution leaks into every prompt.
//
// Invoke still accepts the older `question`/`options` shape, but it is not
// advertised here — the native agent's existing prompts keep working while the
// model-facing contract stays exactly Claude's.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "object",
		Description: "Ask the user up to 4 multiple-choice questions at once.",
		Properties: map[string]*jsonschema.Schema{
			"questions": {
				Type:        "array",
				Description: "The questions to ask. Keep them few and genuinely load-bearing.",
				MinItems:    ptr(1),
				MaxItems:    ptr(4),
				Items: &jsonschema.Schema{
					Type: "object",
					Properties: map[string]*jsonschema.Schema{
						"header": {
							Type:        "string",
							Description: "Short category label for the question (at most 12 characters).",
							MaxLength:   ptr(12),
						},
						"question": {
							Type:        "string",
							Description: "The question to ask the user.",
						},
						"multiSelect": {
							Type:        "boolean",
							Description: "Allow selecting more than one option.",
						},
						"options": {
							Type:        "array",
							Description: "The answer options to offer.",
							MinItems:    ptr(2),
							MaxItems:    ptr(4),
							Items: &jsonschema.Schema{
								Type: "object",
								Properties: map[string]*jsonschema.Schema{
									"label": {
										Type:        "string",
										Description: "A short label for the option.",
									},
									"description": {
										Type:        "string",
										Description: "A longer explanation of what choosing this option means.",
									},
								},
								Required: []string{"label"},
							},
						},
					},
					Required: []string{"header", "question", "options"},
				},
			},
		},
		Required: []string{"questions"},
	}
}

// ptr is the usual helper for the pointer-valued numeric/length constraints in
// jsonschema.Schema.
func ptr[T any](v T) *T { return &v }

// Result is the structured outcome. The MCP frontend JSON-marshals it; the loop
// and CLI render it via String().
//
// The single-question button path sets Choice. The modal-form path sets Answers
// (one entry per question, in order) instead. Either way Answered/Note carry the
// terminal status.
type Result struct {
	Answered bool         `json:"answered"`
	Choice   string       `json:"choice,omitempty"`
	Answers  []FormAnswer `json:"answers,omitempty"`
	Note     string       `json:"note,omitempty"`
	// UserID is who answered. Worth carrying: in a shared channel the person who
	// answers is not always the person who asked, and the model should be able to
	// attribute the decision.
	UserID string `json:"user_id,omitempty"`
}

// FormAnswer is one question's answer in a modal-form Result. Choices holds the
// selected option label(s); Text holds a free-text answer. Exactly one is set.
type FormAnswer struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices,omitempty"`
	Text     string   `json:"text,omitempty"`
}

// String renders the line fed back to the model / shown in the CLI.
func (r Result) String() string {
	if r.Answered {
		if len(r.Answers) > 0 {
			var b strings.Builder
			b.WriteString("The user answered:")
			for _, a := range r.Answers {
				b.WriteString("\n- ")
				b.WriteString(a.Question)
				b.WriteString(": ")
				if a.Text != "" {
					b.WriteString(a.Text)
				} else if len(a.Choices) > 0 {
					b.WriteString(strings.Join(a.Choices, ", "))
				} else {
					b.WriteString("(no answer)")
				}
			}
			return b.String()
		}
		return "The user chose: " + r.Choice
	}
	if r.Note != "" {
		return r.Note
	}
	return "The user did not answer."
}

// Invoke posts the question(s) to the current Slack thread and blocks until the
// user answers (or the wait times out / is cancelled). It routes to the modal
// form when a `questions` array is supplied and demands it (more than one
// question, or any multi-select / free-text question); otherwise it uses the
// single-question button path unchanged.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	if t.broker == nil {
		return nil, fmt.Errorf("Error: interactive questions are not available in this context")
	}
	loc, ok := agent.TurnLocationFromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("Error: the ask tool only works inside a Slack conversation")
	}
	dest := interaction.Destination{ChannelID: loc.ChannelID, ThreadTS: loc.ThreadTS}
	title := strings.TrimSpace(stringArg(args, "title"))

	if questions := parseQuestions(args["questions"]); len(questions) > 0 {
		if needsForm(questions) {
			return t.invokeForm(ctx, dest, title, questions)
		}
		// A single plain single-select question expressed via `questions` still
		// works fine as a button prompt; fold it into the simple path.
		if len(questions) == 1 && strings.TrimSpace(stringArg(args, "question")) == "" {
			args = map[string]any{
				"question": questions[0].Label,
				"options":  optionLabelsAny(questions[0].Options),
				"title":    title,
			}
		}
	}

	question := strings.TrimSpace(stringArg(args, "question"))
	if question == "" {
		return nil, fmt.Errorf("Error: a question is required")
	}
	options := parseOptions(args["options"])
	if len(options) < 2 {
		return nil, fmt.Errorf("Error: provide at least two options")
	}

	decision, err := t.broker.Ask(ctx, dest, interaction.PromptSpec{
		Title:    title,
		Question: question,
		Options:  options,
	})
	if err != nil {
		return nil, err
	}
	switch {
	case decision.TimedOut:
		return Result{Answered: false, Note: "The user did not respond in time. Do not assume an answer — ask again or stop and wait."}, nil
	case decision.Cancelled:
		return Result{Answered: false, Note: "The question was dismissed before the user answered."}, nil
	default:
		return Result{Answered: true, Choice: decision.Label}, nil
	}
}

// invokeForm runs the card path: build a Spec, block on the flow, and shape the
// outcome into a Result listing each question's answer(s).
func (t *Tool) invokeForm(ctx context.Context, dest interaction.Destination, title string, questions []interaction.Question) (any, error) {
	if t.cards == nil {
		return nil, fmt.Errorf("Error: interactive questions are not available in this context")
	}
	spec := askcard.Spec{Title: title, Questions: cardQuestions(questions)}
	resp, err := t.cards.Ask(ctx, askcard.Destination{ChannelID: dest.ChannelID, ThreadTS: dest.ThreadTS}, spec)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.TimedOut:
		return Result{Answered: false, Note: "The user did not respond in time. Do not assume an answer — ask again or stop and wait."}, nil
	case resp.Cancelled:
		return Result{Answered: false, Note: "The questions were dismissed before the user answered."}, nil
	case resp.Chat:
		// The escape hatch. This is NOT a refusal, and must not read like one: the
		// user is declining the offered options and asking to talk it through, so
		// the note is phrased as their question back to the model.
		return Result{Answered: false, Note: chatNote(questions)}, nil
	}
	answers := make([]FormAnswer, 0, len(questions))
	for _, q := range questions {
		answers = append(answers, FormAnswer{Question: q.Label, Choices: resp.Answers[q.Key]})
	}
	return Result{Answered: true, Answers: answers, UserID: resp.UserID}, nil
}

// chatNote is what the model reads when the user presses "Chat About This". It
// restates the questions so the model can open the discussion without having to
// scroll its own transcript for what it asked.
func chatNote(questions []interaction.Question) string {
	var b strings.Builder
	b.WriteString("The user would rather talk this through than pick from the options. They asked: ")
	b.WriteString("\"Can we chat about this?\"\n\nDiscuss these with them before deciding:")
	for _, q := range questions {
		b.WriteString("\n- ")
		b.WriteString(q.Label)
	}
	return b.String()
}

// cardQuestions maps the tool's parsed questions onto the card's own types. The
// card package deliberately does not share types with the interaction broker:
// they answer different shapes (a card of inputs vs a row of buttons).
func cardQuestions(questions []interaction.Question) []askcard.Question {
	out := make([]askcard.Question, 0, len(questions))
	for _, q := range questions {
		opts := make([]askcard.Option, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, askcard.Option{Label: o.Label, Description: o.Description})
		}
		out = append(out, askcard.Question{
			Key:         q.Key,
			Header:      q.Header,
			Question:    q.Label,
			Options:     opts,
			MultiSelect: q.MultiSelect,
		})
	}
	return out
}

func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// needsForm reports whether the questions require the card: more than one
// question, any multi-select, or any option carrying a description (which a
// button has nowhere to show). A lone plain single-select question can still ride
// the simpler button path.
func needsForm(questions []interaction.Question) bool {
	if len(questions) > 1 {
		return true
	}
	for _, q := range questions {
		if q.MultiSelect || q.Header != "" {
			return true
		}
		for _, o := range q.Options {
			if o.Description != "" {
				return true
			}
		}
	}
	return false
}

// parseQuestions reads the `questions` array into interaction.Question values.
// Each gets a stable key (q0, q1, …) so answers round-trip through the card.
//
// The question text is read from `question` (Claude's field) or `label` (the
// older Murtaugh one). Accepting both is what lets the advertised schema be
// exactly Claude's without breaking prompts already written against `label`.
func parseQuestions(raw any) []interaction.Question {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]interaction.Question, 0, len(list))
	for i, v := range list {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		label := strings.TrimSpace(stringArg(m, "question"))
		if label == "" {
			label = strings.TrimSpace(stringArg(m, "label"))
		}
		if label == "" {
			continue
		}
		out = append(out, interaction.Question{
			Key:         fmt.Sprintf("q%d", i),
			Header:      strings.TrimSpace(stringArg(m, "header")),
			Label:       label,
			Options:     parseOptions(m["options"]),
			MultiSelect: boolArg(m, "multiSelect"),
		})
	}
	return out
}

func boolArg(args map[string]any, key string) bool {
	b, _ := args[key].(bool)
	return b
}

// optionLabelsAny re-expands parsed options into the []any of strings the simple
// button path's parseOptions expects.
func optionLabelsAny(opts []interaction.Option) []any {
	out := make([]any, 0, len(opts))
	for _, o := range opts {
		out = append(out, o.Label)
	}
	return out
}

// parseOptions reads an options array in either shape: Claude's objects
// ({label, description}) or the older bare strings. An object without a usable
// label is dropped rather than rendering a blank, unpickable choice.
func parseOptions(raw any) []interaction.Option {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]interaction.Option, 0, len(list))
	for _, v := range list {
		switch opt := v.(type) {
		case string:
			if s := strings.TrimSpace(opt); s != "" {
				out = append(out, interaction.Option{ID: s, Label: s})
			}
		case map[string]any:
			label := strings.TrimSpace(stringArg(opt, "label"))
			if label == "" {
				continue
			}
			out = append(out, interaction.Option{
				ID:          label,
				Label:       label,
				Description: strings.TrimSpace(stringArg(opt, "description")),
			})
		}
	}
	return out
}
