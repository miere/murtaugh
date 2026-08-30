// Package configcard renders the configuration-change approval card and owns
// the stable action_id values the gateway's interaction router keys on.
//
// The card is deliberately its own thing rather than a reuse of the `ask`
// tool's prompt. `ask` renders a question with a row of options; this renders a
// diff, states a consequence, and drives a two-way decision that mutates the
// config store either way. Squeezing it into `ask` would mean teaching that
// tool about diffs and rollbacks it has no other reason to know, and every
// future `ask` caller would carry the weight.
//
// Like the other container-block cards it is a Block Kit JSON template rendered
// through internal/jsontemplate and posted via the client's raw-blocks
// passthrough, because `container` and `rich_text_preformatted` are newer than
// the pinned slack-go — see "Block Kit rendering" in ARCHITECTURE.md.
package configcard

import (
	"fmt"
	"io/fs"
	"strings"

	"github.com/miere/murtaugh/internal/jsontemplate"
)

const (
	// ContainerBlockID tags the card's container block.
	ContainerBlockID = "murtaugh_config_update_card"

	// ActionPrefix namespaces every action_id this card emits. The gateway
	// recognises a config interaction by this prefix, so the router never has
	// to match on free-form text.
	//
	// Deliberately not a prefix of ContainerBlockID. An earlier spelling
	// ("murtaugh_config_update_") was a substring of the block id, so any
	// contains-style check for a live button also matched the container of a
	// settled card. Nothing routed on that, but a naming collision between an
	// identifier that means "click me" and one that means "this is the card" is
	// a trap worth not leaving lying around.
	ActionPrefix = "murtaugh_config_action_"
)

// Action is which button was pressed.
type Action string

const (
	// ActionApply adopts the edited configuration and reloads.
	ActionApply Action = "apply"
	// ActionRollback restores the running configuration over the edit.
	ActionRollback Action = "rollback"
)

// Template is the card's template reference, resolved against the config dir
// first and the embedded assets tree second, so an operator can restyle it
// without a rebuild.
const Template = "templates/config/update.json"

// Card headings. They are constants rather than caller-supplied because every
// instance of this card says the same thing — only the diff changes.
const (
	Title    = "Configuration Update"
	Subtitle = "The configuration has been updated and I need your approval"
	Intro    = "Here is a diff of what has changed on your configuration for your review."
)

// Impact is the sentence warning what applying will cost.
//
// It is not optional and not soft-pedalled. A soft reload rebuilds every agent,
// which tears down the backend process trees and stops whatever they were
// mid-way through; an admin who approves without being told that has been
// misled by the card, however reasonable the change looked.
const Impact = "Applying will reload every agent: any conversation or job in progress will be stopped."

// ActionID builds the action_id for one button of one pending change.
//
// The correlation id is carried in the action_id rather than in the button
// value because Slack returns action_id on every interaction shape, and it is
// what lets a click find the decision it belongs to after a card has been
// sitting in a DM for a while.
func ActionID(corr string, a Action) string {
	return ActionPrefix + string(a) + "_" + corr
}

// ParseActionID pulls the action and correlation id back out of an action_id.
func ParseActionID(id string) (corr string, action Action, ok bool) {
	rest, found := strings.CutPrefix(id, ActionPrefix)
	if !found {
		return "", "", false
	}
	for _, candidate := range []Action{ActionApply, ActionRollback} {
		if corr, found := strings.CutPrefix(rest, string(candidate)+"_"); found && corr != "" {
			return corr, candidate, true
		}
	}
	return "", "", false
}

// Data is the template's view model.
type Data struct {
	Title    string
	Subtitle string
	Intro    string
	// Diff is the rendered change, WITHOUT file or hunk headers. See
	// config.DiffSnapshots for why.
	Diff string
	// Impact is the consequence sentence; empty omits the line.
	Impact string
	// ShowActions is false once the card has been settled, so a decided card
	// cannot be clicked again.
	ShowActions    bool
	ActionApply    string
	ActionRollback string
	// Footer records the settled outcome, replacing the buttons.
	Footer string
}

// Renderer builds the card from the template tree.
type Renderer struct{ tpl *jsontemplate.Renderer }

// NewRenderer returns a Renderer reading templates from dir first and fsys
// second, matching every other card in this tree.
func NewRenderer(dir string, fsys fs.FS) *Renderer {
	return &Renderer{tpl: jsontemplate.New(dir, fsys)}
}

// Pending renders the card as an open decision.
func (r *Renderer) Pending(corr, diff string) ([]byte, error) {
	return r.render(Data{
		Title:          Title,
		Subtitle:       Subtitle,
		Intro:          Intro,
		Diff:           diff,
		Impact:         Impact,
		ShowActions:    true,
		ActionApply:    ActionID(corr, ActionApply),
		ActionRollback: ActionID(corr, ActionRollback),
	})
}

// Settled renders the card with its buttons replaced by the outcome, so the
// admin's DM keeps the record of what was decided rather than a live-looking
// prompt that no longer does anything.
func (r *Renderer) Settled(diff, footer string) ([]byte, error) {
	return r.render(Data{
		Title:    Title,
		Subtitle: Subtitle,
		Intro:    Intro,
		Diff:     diff,
		Footer:   footer,
	})
}

func (r *Renderer) render(data Data) ([]byte, error) {
	blocks, err := r.tpl.Render(Template, data)
	if err != nil {
		return nil, fmt.Errorf("render config update card: %w", err)
	}
	return blocks, nil
}

// PlainText is the notification fallback and the degraded rendering used when
// no raw-blocks client is available. The diff is fenced so it survives as a
// code block even without the card.
func PlainText(diff string) string {
	var b strings.Builder
	b.WriteString(":gear: *" + Title + "*\n")
	b.WriteString(Subtitle + "\n\n")
	b.WriteString(Intro + "\n```\n")
	b.WriteString(diff)
	if !strings.HasSuffix(diff, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("```\n")
	b.WriteString(Impact)
	return b.String()
}
