// Package agentcard renders the prompt a fresh Murtaugh sends when it has no
// agent configured, and owns the identifiers its button and modal are routed
// by.
//
// It exists because the install now finishes in Slack. A daemon with no agent
// is running correctly and answering nobody, which from the operator's side is
// indistinguishable from broken — so it says so, and hands them the form.
package agentcard

import (
	"fmt"
	"io/fs"

	"github.com/miere/murtaugh/internal/jsontemplate"
)

const (
	// ContainerBlockID tags the card's container block.
	ContainerBlockID = "murtaugh_agent_setup_card"

	// ActionOpen is the button that opens the setup form.
	ActionOpen = "murtaugh_agent_setup_open"

	// ModalCallbackID tags the setup modal so its submissions can be routed.
	// Every step of the form carries it: the step itself lives in the view's
	// private_metadata, so one callback id serves the whole conversation.
	ModalCallbackID = "murtaugh_agent_setup_modal"
)

// Template is the card's template reference, resolved against the config dir
// first and the embedded assets tree second.
const Template = "templates/agent/setup.json"

// Card copy. Constant because every instance says the same thing.
const (
	Title    = "Murtaugh has no agent configured"
	Subtitle = "Chat is off until one exists, so nobody can talk to me yet."
	Body     = "I am installed, connected to Slack, and holding the leader lock — but with no agent profile there is nothing to answer with.\n\nSetting one up creates two profiles: a general one for everybody, and a `tweaker` for your own DMs that can change my configuration."
)

// Data is the template's view model.
type Data struct {
	Title      string
	Subtitle   string
	Body       string
	ShowAction bool
	ActionOpen string
	// Footer replaces the button once the form has been completed, so a stale
	// card in the DM cannot be clicked into a second setup.
	Footer string
}

// Renderer builds the card from the template tree.
type Renderer struct{ tpl *jsontemplate.Renderer }

// NewRenderer returns a Renderer reading templates from dir first and fsys
// second, matching every other card in this tree.
func NewRenderer(dir string, fsys fs.FS) *Renderer {
	return &Renderer{tpl: jsontemplate.New(dir, fsys)}
}

// Prompt renders the card with its button live.
func (r *Renderer) Prompt() ([]byte, error) {
	return r.render(Data{
		Title:      Title,
		Subtitle:   Subtitle,
		Body:       Body,
		ShowAction: true,
		ActionOpen: ActionOpen,
	})
}

// Settled renders the card with the button replaced by an outcome.
func (r *Renderer) Settled(footer string) ([]byte, error) {
	return r.render(Data{Title: Title, Subtitle: Subtitle, Body: Body, Footer: footer})
}

func (r *Renderer) render(data Data) ([]byte, error) {
	blocks, err := r.tpl.Render(Template, data)
	if err != nil {
		return nil, fmt.Errorf("render agent setup card: %w", err)
	}
	return blocks, nil
}

// PlainText is the notification fallback and the degraded rendering used when
// no raw-blocks client is available.
func PlainText() string {
	return ":warning: *" + Title + "*\n" + Subtitle + "\n\n" +
		"Open Murtaugh's App Home, or re-run setup, to configure one."
}
