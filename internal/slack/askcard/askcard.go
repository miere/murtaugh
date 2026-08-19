// Package askcard renders and routes the multi-question card the `ask` tool puts
// in front of a user.
//
// It replaces a modal. A modal cannot be opened cold — views.open needs a fresh
// trigger_id from a user interaction — so the old flow cost three hops: post an
// "Answer" button, open the modal on the click, read a view_submission. Slack
// accepts `input` blocks directly in a message, and a block_actions payload
// carries their state, so the card is now posted once and answered in place.
//
// The cards are Block Kit JSON templates under assets/templates/ask, rendered
// through internal/jsontemplate and posted verbatim via the client's raw-blocks
// passthrough. They use block types (container, callout) newer than the pinned
// slack-go, which is why they are templates rather than Go builders — see
// "Block Kit rendering" in ARCHITECTURE.md.
//
// Correlation mirrors internal/slack/authcard: a random id minted per ask,
// carried in the buttons' action_id namespace, recognised by the gateway router
// and handed back here.
package askcard

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"strings"
	"time"

	slackgo "github.com/slack-go/slack"

	"github.com/miere/murtaugh/internal/jsontemplate"
)

const (
	// BlockID tags the card's actions block. The gateway router recognises an ask
	// interaction by it or by the action_id prefix.
	BlockID = "murtaugh_ask"

	// ActionPrefix namespaces every action_id the card emits. The correlation id
	// and the action name are appended: "murtaugh_ask:<corr>:<action>".
	ActionPrefix = "murtaugh_ask:"

	// inputPrefix namespaces each question's input block_id/action_id so the
	// question key can be recovered from the payload's state values.
	inputPrefix = "murtaugh_ask_q:"
)

// Template references, resolved against the config dir first and the embedded
// assets tree second — so an operator can restyle a card without a rebuild.
const (
	PendingTemplate  = "templates/ask/pending.json"
	AnsweredTemplate = "templates/ask/answered.json"
	ChatTemplate     = "templates/ask/chat.json"
)

// DefaultTimeout bounds one ask. While it blocks, the backend's tool heartbeat
// keeps the gateway's idle watchdog fed, so this — not the watchdog — is the
// governing bound.
const DefaultTimeout = 10 * time.Minute

// Action is one button on the card.
type Action string

const (
	// ActionSubmit collects the answers. It resolves the ask only when every
	// question has one; otherwise the card is re-rendered with a validation
	// callout and the user's partial answers intact.
	ActionSubmit Action = "submit"
	// ActionChat is the escape hatch: the user would rather discuss than pick
	// from the offered options. It resolves the ask as a request to talk, which
	// the tool hands back to the model as a question.
	ActionChat Action = "chat"
)

// State is what the card is currently showing. The terminal states each have
// their own template; pending is the only one with inputs.
type State string

const (
	StatePending   State = "pending"
	StateAnswered  State = "answered"
	StateChat      State = "chat"
	StateTimeout   State = "timeout"
	StateCancelled State = "cancelled"
)

// Option is one selectable answer.
type Option struct {
	// Label is the option's short name and its stable value: it is what the
	// model gets back, so it must survive the round trip unchanged.
	Label string
	// Description is the longer explanation shown beneath the label. Optional.
	Description string
}

// Question is one prompt on the card.
type Question struct {
	// Key identifies the question in the payload's state values. Assigned by the
	// caller (or defaulted to q0, q1, … by Ask).
	Key string
	// Header is the short category label ("Storage Engine"), rendered ahead of
	// the question text. Optional.
	Header string
	// Question is the prompt itself.
	Question string
	// Options are the offered answers.
	Options []Option
	// MultiSelect renders checkboxes instead of radio buttons.
	MultiSelect bool
}

// Spec describes one ask.
type Spec struct {
	Title     string
	Questions []Question
	Timeout   time.Duration // 0 → DefaultTimeout
}

// Response is the outcome of an Ask. Exactly one of Submitted/Chat/TimedOut/
// Cancelled describes how it ended.
type Response struct {
	// Answers maps each question key to the chosen option labels — one entry for
	// a single-select question, one or more for a multi-select.
	Answers map[string][]string
	// UserID is who answered.
	UserID string

	Submitted bool
	// Chat reports that the user pressed "Chat About This" rather than
	// answering. It is not a refusal: the tool turns it into a question back to
	// the model so the conversation continues.
	Chat      bool
	TimedOut  bool
	Cancelled bool
}

// Answered reports whether the user actually submitted answers.
func (r Response) Answered() bool { return r.Submitted && !r.Chat && !r.TimedOut && !r.Cancelled }

// Destination is the Slack conversation the card is posted to.
type Destination struct {
	ChannelID string
	ThreadTS  string
}

// ActionID builds the action_id for one button of one ask.
func ActionID(corr string, a Action) string {
	return ActionPrefix + corr + ":" + string(a)
}

// ParseActionID pulls the correlation id and action back out of an action_id.
// ok is false for any id outside this package's namespace.
func ParseActionID(id string) (corr string, action Action, ok bool) {
	if !strings.HasPrefix(id, ActionPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(id, ActionPrefix)
	i := strings.LastIndex(rest, ":")
	if i <= 0 {
		return "", "", false
	}
	return rest[:i], Action(rest[i+1:]), true
}

// IsAskInteraction reports whether ic is a click on an ask card, returning the
// correlation id and which button. The gateway uses it to dispatch.
func IsAskInteraction(ic slackgo.InteractionCallback) (corr string, action Action, ok bool) {
	if ic.Type != slackgo.InteractionTypeBlockActions {
		return "", "", false
	}
	for _, a := range ic.ActionCallback.BlockActions {
		if a == nil {
			continue
		}
		if corr, action, ok = ParseActionID(a.ActionID); ok {
			return corr, action, true
		}
	}
	return "", "", false
}

// ParseSubmission reads the selected option values out of a block_actions
// payload, keyed by question key.
//
// This is the whole reason the modal could be dropped: for a message carrying
// `input` blocks, Slack sends every input's current state alongside the click,
// so one button press delivers the entire form. A view_submission is no longer
// needed, and neither is the trigger_id that used to gate it.
func ParseSubmission(ic slackgo.InteractionCallback) map[string][]string {
	answers := map[string][]string{}
	if ic.BlockActionState == nil {
		return answers
	}
	for blockID, actions := range ic.BlockActionState.Values {
		key := strings.TrimPrefix(blockID, inputPrefix)
		if key == blockID {
			continue // not one of ours
		}
		for _, action := range actions {
			switch {
			case len(action.SelectedOptions) > 0: // checkboxes
				labels := make([]string, 0, len(action.SelectedOptions))
				for _, opt := range action.SelectedOptions {
					labels = append(labels, opt.Value)
				}
				answers[key] = labels
			case action.SelectedOption.Value != "": // radio buttons
				answers[key] = []string{action.SelectedOption.Value}
			}
		}
	}
	return answers
}

// unanswered lists, in order, the questions with no selection in answers. Slack
// does not enforce `optional: false` outside a modal — a message's Submit button
// fires no matter what is filled in — so the check has to happen here.
func unanswered(questions []Question, answers map[string][]string) []Question {
	var missing []Question
	for _, q := range questions {
		if len(answers[q.Key]) == 0 {
			missing = append(missing, q)
		}
	}
	return missing
}

// cardData is the context the templates render against. Field names are part of
// the template contract: renaming one breaks any operator override.
type cardData struct {
	Title     string
	Subtitle  string
	State     string
	Questions []questionData

	// ValidationError is the callout shown above a re-rendered pending card when
	// Submit arrived incomplete. Empty renders no callout.
	ValidationError string
	// ShowActions gates the Submit/Chat buttons: only a pending card has them.
	ShowActions bool
	// AnsweredBy is the user id rendered on a terminal card ("" omits it).
	AnsweredBy string

	ActionSubmit string
	ActionChat   string
}

type questionData struct {
	Key         string
	BlockID     string
	ActionID    string
	Label       string
	MultiSelect bool
	Options     []optionData
	// Answers are the labels chosen for this question, for the answered card.
	Answers []string
}

type optionData struct {
	// Value round-trips the option's label through Slack, so it is what comes
	// back in the submission.
	Value string
	// Text is the rendered mrkdwn line: the label, then the description.
	Text string
	// Selected drives initial_option/initial_options. Without it a re-render
	// (the validation path) would wipe every answer the user had already given.
	Selected bool
}

// label renders the input's label: "1. Header - Question", or just the question
// when no header was supplied. The number orients the user when the callout says
// which questions are still missing.
func (q Question) label(i int) string {
	text := strings.TrimSpace(q.Question)
	if h := strings.TrimSpace(q.Header); h != "" {
		text = h + " - " + text
	}
	return fmt.Sprintf("%d. %s", i+1, text)
}

// optionText renders one option as mrkdwn: the label italicised, then the
// description. Slack's radio/checkbox options do carry a native `description`
// field, but it renders in a dimmer, smaller style that buries a description
// doing real explanatory work — which is exactly what these carry.
func (o Option) optionText() string {
	label := strings.TrimSpace(o.Label)
	if d := strings.TrimSpace(o.Description); d != "" {
		return "_" + label + "_ - " + d
	}
	return "_" + label + "_"
}

// Renderer turns the card templates into the raw Block Kit JSON the Slack client
// posts verbatim.
type Renderer struct {
	tpl *jsontemplate.Renderer
}

// NewRenderer builds a Renderer resolving templates from dir first, then fsys.
func NewRenderer(dir string, fsys fs.FS) *Renderer {
	return &Renderer{tpl: jsontemplate.New(dir, fsys)}
}

func (r *Renderer) render(ref string, data cardData) ([]byte, error) {
	out, err := r.tpl.Render(ref, data)
	if err != nil {
		return nil, fmt.Errorf("askcard: render %s: %w", ref, err)
	}
	return out, nil
}

// templateFor maps a state to the card that renders it. Timeout and cancelled
// reuse the chat card: all three end without answers, and the card's job is then
// to say what was asked and that it went unanswered.
func templateFor(state State) string {
	switch state {
	case StateAnswered:
		return AnsweredTemplate
	case StatePending:
		return PendingTemplate
	default:
		return ChatTemplate
	}
}

// subtitleFor is the one-line summary under the card title.
func subtitleFor(state State, n int, missing int) string {
	switch state {
	case StatePending:
		if missing > 0 {
			return fmt.Sprintf("%s still need an answer.", plural(missing, "question", "questions"))
		}
		return fmt.Sprintf("We need your input on the following %s.", plural(n, "question", "questions"))
	case StateAnswered:
		return fmt.Sprintf("The user answered %s.", plural(n, "question", "questions"))
	case StateChat:
		return fmt.Sprintf("The user wants to chat about the %s asked.", plural(n, "question", "questions"))
	case StateTimeout:
		return fmt.Sprintf("No answer to the %s asked.", plural(n, "question", "questions"))
	default:
		return fmt.Sprintf("The %s were dismissed before being answered.", plural(n, "question", "questions"))
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// fallbackText is the notification line shown where blocks cannot render — a
// push notification, a screen reader, the sidebar preview.
func fallbackText(spec Spec) string {
	if t := strings.TrimSpace(spec.Title); t != "" {
		return t
	}
	if len(spec.Questions) > 0 {
		return clamp(strings.TrimSpace(spec.Questions[0].Question), 100)
	}
	return "Your input is needed"
}

func clamp(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-1]) + "…"
}

func newCorrelationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("askcard: mint correlation id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
