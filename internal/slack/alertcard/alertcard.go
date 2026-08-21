// Package alertcard renders the alerts Murtaugh sends a user about itself — a
// failure, a warning, or a statement of fact — as one collapsed container block:
// a severity icon, a headline that reads on its own, and a body that stays
// folded until somebody asks for it.
//
// Collapsing is the point. These alerts interrupt a conversation the user cares
// about, and the worst of them (a provider 503 arrives as a wall of JSON) used
// to be pasted inline. A container keeps the title and subtitle visible while
// the body is folded, so the alert costs one line of screen space and the full
// diagnostic text is one click away rather than truncated out of existence.
//
// It renders only, and it owns no lifecycle: a caller posts, updates and
// journals its own alert. That is what separates this from internal/slack/askcard
// and internal/slack/authcard, whose cards are stateful conversations.
//
// # Scope
//
// This covers the NON-INTERACTIVE alerts — the ones that were bare text. The
// alerts that already render as cards with their own routing contracts
// (internal/slack/approvalcard, restartcard, askcard, authcard) keep their own
// rendering: folding them in here would re-plumb click routers for no visual
// gain. Those are a separate pass.
//
// The card is a Block Kit JSON template under assets/templates/alert, rendered
// through internal/jsontemplate and posted verbatim via the client's raw-blocks
// passthrough. It uses block types (container, rich_text) newer than the pinned
// slack-go, which is why it is a template rather than a Go builder — see
// "Block Kit rendering" in ARCHITECTURE.md.
package alertcard

import (
	"fmt"
	"io/fs"
	"strings"

	"github.com/miere/murtaugh/internal/jsontemplate"
)

// Level is how loud an alert is. The vocabulary is deliberately the journal's
// (internal/journal.Level): an alert shown in Slack and the event recorded for
// it should use the same word, so an operator reading the journal is reading
// the same severity the user saw.
//
// The four buckets of the message audit map onto these three: a real failure is
// LevelError; a real warning is LevelWarn; a refusal splits, with "you asked and
// the answer is no" reading as LevelWarn and "this deployment does not have that
// feature" as LevelInfo, because nothing went wrong in the second case.
type Level string

const (
	// LevelError is something broke and the thing the user asked for did not
	// happen.
	LevelError Level = "error"
	// LevelWarn is nothing crashed, but the user needs to know and may need to
	// act.
	LevelWarn Level = "warn"
	// LevelInfo is a statement of fact — a capability that is not configured, a
	// state that is not an error.
	LevelInfo Level = "info"
)

// ContainerBlockID tags the card's container block. It is descriptive only:
// nothing routes on it, because an alert carries no buttons. It exists so an
// alert is identifiable in a Block Kit payload during debugging.
const ContainerBlockID = "murtaugh_alert_card"

// Template is the card's template reference, resolved against the config dir
// first and the embedded assets tree second — so an operator can restyle alerts,
// including the "*Reason*" and "*Next Steps*" labels, without a rebuild.
const Template = "templates/alert/alert.json"

// Icon URLs, one per level. Every URL here was supplied from the canvas
// mock-ups rather than invented, so each one is already known to render in
// Slack — the same discipline internal/slack/approvalcard follows.
const (
	iconError = "https://img.icons8.com/external-flaticons-lineal-color-flat-icons/64/external-close-button-web-flaticons-lineal-color-flat-icons.png"
	iconWarn  = "https://img.icons8.com/external-flaticons-lineal-color-flat-icons/64/external-danger-electrician-flaticons-lineal-color-flat-icons-4.png"
	iconInfo  = "https://img.icons8.com/external-flaticons-lineal-color-flat-icons/64/external-help-dating-app-flaticons-lineal-color-flat-icons-4.png"
)

// Slack's limits on the text objects this card is built from, and how the body
// budget is divided.
//
// The body is assembled by the TEMPLATE from Reason, Text and NextSteps, so no
// single Go value maps to the 3000-rune section limit. Rather than compose the
// string here (which would move the labels out of the template and cost the
// operator their restyling hook), each part gets a share of the budget. The
// shares reflect what each field is for: Reason and NextSteps are a phrase and a
// sentence, Text is the one that carries prose. They sum to 2800, leaving room
// for the labels and blank lines the template adds.
const (
	titleLimit     = 150  // a container title (plain_text)
	subtitleLimit  = 150  // a container subtitle (plain_text)
	reasonLimit    = 500  // "*Reason*: …"
	nextStepsLimit = 500  // "*Next Steps*: …"
	textLimit      = 1800 // free prose between the two
	detailLimit    = 3000 // the preformatted block, which is its own child block
)

// Spec is one alert. Only Level is required; every other field is optional and
// an empty one renders nothing at all rather than an empty box.
//
// The body fields exist separately rather than as one blob of markdown because
// the split is the whole point of normalising these messages: before this, a
// user got a raw Go error chain and no guidance. Reason names what happened,
// NextSteps says what to do about it, and Detail carries the unabridged text for
// whoever is debugging.
type Spec struct {
	// Level picks the icon and the default title. An unset or unrecognised
	// Level is treated as LevelError — see normalise.
	Level Level

	// Title is the headline, visible while the card is collapsed. Empty takes
	// the default for the level.
	Title string
	// Subtitle is the one-line summary next to the title, also visible while
	// collapsed. This is the field that should read as a whole message on its
	// own, because most users will never expand the card.
	Subtitle string

	// Reason is the short "why", rendered after a bold "*Reason*:" label — a
	// stop reason, a status code, a provider's own sentence.
	Reason string
	// Text is free mrkdwn between the reason and the next steps, for an alert
	// whose body is prose rather than a reason/remedy pair.
	Text string
	// NextSteps is what the user should do, rendered after a bold
	// "*Next Steps*:" label.
	NextSteps string

	// Detail is the unabridged diagnostic — a raw error chain, a provider's
	// JSON body — rendered as a preformatted block. Safe to fill generously:
	// the card is collapsed, so this costs no screen space until expanded.
	Detail string
}

// data is what the template renders against. Field names are part of the
// template contract: renaming one breaks any operator override.
type data struct {
	IconURL   string
	IconAlt   string
	Title     string
	Subtitle  string
	Reason    string
	Text      string
	NextSteps string
	Detail    string
}

// Renderer turns a Spec into the raw Block Kit JSON the Slack client posts
// verbatim.
type Renderer struct {
	tpl *jsontemplate.Renderer
}

// NewRenderer builds a Renderer resolving templates from dir first, then fsys.
func NewRenderer(dir string, fsys fs.FS) *Renderer {
	return &Renderer{tpl: jsontemplate.New(dir, fsys)}
}

// Render returns the card's blocks. It never fails on the content of a Spec —
// every field is clamped and defaulted rather than rejected, because an alert
// that cannot render is an alert the user never sees, and these fire on the
// paths where something has already gone wrong.
func (r *Renderer) Render(spec Spec) ([]byte, error) {
	out, err := r.tpl.Render(Template, templateData(spec))
	if err != nil {
		return nil, fmt.Errorf("alertcard: render %s: %w", Template, err)
	}
	return out, nil
}

// templateData resolves a Spec into the rendering contract: level defaults
// applied, every value clamped, and a guaranteed-non-empty body.
func templateData(spec Spec) data {
	spec = normalise(spec)
	return data{
		IconURL:   icon(spec.Level),
		IconAlt:   iconAlt(spec.Level),
		Title:     clamp(spec.Title, titleLimit),
		Subtitle:  clamp(spec.Subtitle, subtitleLimit),
		Reason:    clamp(spec.Reason, reasonLimit),
		Text:      clamp(spec.Text, textLimit),
		NextSteps: clamp(spec.NextSteps, nextStepsLimit),
		Detail:    clamp(spec.Detail, detailLimit),
	}
}

// normalise fills in what the caller left out: a known level, a title, and a
// body. Everything it does is data, not structure — the template still decides
// how the pieces are laid out.
func normalise(spec Spec) Spec {
	spec.Level = normaliseLevel(spec.Level)
	spec.Title = strings.TrimSpace(spec.Title)
	spec.Subtitle = strings.TrimSpace(spec.Subtitle)
	spec.Reason = strings.TrimSpace(spec.Reason)
	spec.Text = strings.TrimSpace(spec.Text)
	spec.NextSteps = strings.TrimSpace(spec.NextSteps)
	// Trailing newlines only: leading whitespace in a diagnostic can be
	// meaningful indentation.
	spec.Detail = strings.TrimRight(spec.Detail, "\n")

	if spec.Title == "" {
		spec.Title = defaultTitle(spec.Level)
	}
	// A collapsible card with nothing inside is a trap: the user clicks to
	// expand and finds an empty box. When a caller supplies no body at all,
	// the level's generic guidance becomes the body, which is both honest and
	// the most useful thing left to say.
	if spec.Reason == "" && spec.Text == "" && spec.NextSteps == "" && spec.Detail == "" {
		spec.NextSteps = defaultNextSteps(spec.Level)
	}
	return spec
}

// normaliseLevel maps an unset or unrecognised level onto LevelError. The zero
// value only occurs when a caller forgot to set one, and over-reporting a
// severity is recoverable in a way that silently downgrading a real failure to
// an informational note is not.
func normaliseLevel(level Level) Level {
	switch level {
	case LevelError, LevelWarn, LevelInfo:
		return level
	default:
		return LevelError
	}
}

func icon(level Level) string {
	switch level {
	case LevelWarn:
		return iconWarn
	case LevelInfo:
		return iconInfo
	default:
		return iconError
	}
}

func iconAlt(level Level) string {
	switch level {
	case LevelWarn:
		return "Warning icon"
	case LevelInfo:
		return "Information icon"
	default:
		return "Error icon"
	}
}

// defaultTitle is the headline for a caller that supplies none. The error
// wording is the canvas mock's.
func defaultTitle(level Level) string {
	switch level {
	case LevelWarn:
		return "Heads up"
	case LevelInfo:
		return "Good to know"
	default:
		return "Oops! Something went wrong!"
	}
}

// defaultNextSteps is the body of last resort, used only when a caller supplies
// no body at all.
func defaultNextSteps(level Level) string {
	switch level {
	case LevelWarn:
		return "Nothing to do right now — worth a look if it keeps happening."
	case LevelInfo:
		return "No action needed."
	default:
		return "Try again. If it keeps happening, notify your admin user."
	}
}

// clamp shortens s to limit runes, appending an ellipsis so the truncation is
// visible. It counts runes rather than bytes so multibyte text is never cut
// mid-character.
//
// Every field of a Spec is of unbounded length in practice — a provider error
// body, a Go error chain — and exceeding a Slack text limit makes the API reject
// the whole message with invalid_blocks. On this path that would mean an alert
// about a failure itself failing to arrive.
func clamp(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-1]) + "…"
}

// FallbackText is the one-line notification form: what Slack shows in a push
// notification, the sidebar preview and a screen reader, and what belongs in a
// posted message's text field alongside the blocks.
//
// It is title and subtitle only. A push notification is the least private
// surface Slack has, so the diagnostic detail stays in the card.
func FallbackText(spec Spec) string {
	spec = normalise(spec)
	if spec.Subtitle == "" {
		return spec.Title
	}
	return spec.Title + " — " + spec.Subtitle
}

// PlainText renders the whole alert as mrkdwn, for the surfaces that cannot host
// blocks at all: a message appended into an in-flight stream, and a canvas if it
// turns out not to accept Block Kit. It is the same content as the card in the
// same order, minus the container.
//
// Callers that can post blocks should post blocks — this is the degradation
// path, not an alternative style.
func PlainText(spec Spec) string {
	spec = normalise(spec)

	var b strings.Builder
	b.WriteString(Marker(spec.Level) + " *" + clamp(spec.Title, titleLimit) + "*")
	if spec.Subtitle != "" {
		b.WriteString("\n" + clamp(spec.Subtitle, subtitleLimit))
	}
	if spec.Reason != "" {
		b.WriteString("\n\n*Reason*: " + clamp(spec.Reason, reasonLimit))
	}
	if spec.Text != "" {
		b.WriteString("\n\n" + clamp(spec.Text, textLimit))
	}
	if spec.NextSteps != "" {
		b.WriteString("\n\n*Next Steps*: " + clamp(spec.NextSteps, nextStepsLimit))
	}
	if spec.Detail != "" {
		b.WriteString("\n```\n" + clamp(spec.Detail, detailLimit) + "\n```")
	}
	return b.String()
}

// Marker is the emoji stand-in for the level icon, since the image URLs the card
// uses have no meaning outside Block Kit.
//
// It is exported for the surfaces where a card is the wrong shape but the
// severity vocabulary should still be shared — notably an ephemeral slash-command
// ack, which is one transient line only its invoker sees. Those keep their single
// sentence and just gain the marker, so "not authorized" reads at the same
// severity whether it arrives as an ack or a card.
func Marker(level Level) string {
	switch normaliseLevel(level) {
	case LevelWarn:
		return ":warning:"
	case LevelInfo:
		return ":information_source:"
	default:
		return ":x:"
	}
}
