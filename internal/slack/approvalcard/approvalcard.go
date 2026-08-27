// Package approvalcard renders the card an approval request puts in front of a
// user: one container block showing what wants to run and, when there is one,
// the command it wants to run, over a row of decision buttons.
//
// It renders only. Unlike internal/slack/askcard, this package owns no
// lifecycle: posting, correlation, the timeout, the terminal rewrite and the
// optional chat.delete all stay in internal/slack/interaction's Broker, which
// already drove the pre-card approval prompts for all three gates (the native
// terminal gate, the ACP permission gate, and the claude_code gate that routes
// through the ACP one) and the scheduler's first-run hold. The Broker calls in
// here through its CardRenderer hook.
//
// The cards are Block Kit JSON templates under assets/templates/approval,
// rendered through internal/jsontemplate and posted verbatim via the client's
// raw-blocks passthrough. They use block types (container, rich_text) newer than
// the pinned slack-go, which is why they are templates rather than Go builders —
// see "Block Kit rendering" in ARCHITECTURE.md.
package approvalcard

import (
	"fmt"
	"io/fs"
	"strings"

	"github.com/miere/murtaugh/internal/jsontemplate"
)

// ContainerBlockID tags the card's container block. It is descriptive only: the
// gateway router correlates a click by the action_id its buttons carry, and by
// the *actions* block_id, which the caller supplies (see Data.ActionsBlockID) so
// this package never has to know the broker's routing constants.
const ContainerBlockID = "murtaugh_approval_card"

// Template references, resolved against the config dir first and the embedded
// assets tree second — so an operator can restyle a card without a rebuild.
const (
	PendingTemplate  = "templates/approval/pending.json"
	ResolvedTemplate = "templates/approval/resolved.json"
)

// Icon URLs. Both are taken from the canvas mock-ups rather than invented, so
// every URL shipped here is one that has already been seen to render in Slack.
const (
	// iconPending is the mock's "protocols" icon, used for a request awaiting a
	// decision and for one that was granted.
	iconPending = "https://img.icons8.com/external-flaticons-lineal-color-flat-icons/64/external-protocols-back-to-work-flaticons-lineal-color-flat-icons.png"
	// iconRefused marks the outcomes where the tool did not run. It is the same
	// warning icon the canvas uses for the alert card.
	iconRefused = "https://img.icons8.com/external-flaticons-lineal-color-flat-icons/64/external-warning-winter-season-flaticons-lineal-color-flat-icons-2.png"
)

// Outcome is how an approval ended. The canvas specified only "Approved", but
// the gates produce four terminal states and each needs to read differently —
// a denial and a timeout are not the same event, and only one of them means
// somebody actually said no.
type Outcome string

const (
	// OutcomeApproved covers every allow_* choice, including "approve & always
	// allow": what the card reports is that the tool ran.
	OutcomeApproved Outcome = "approved"
	// OutcomeDenied is a human declining.
	OutcomeDenied Outcome = "denied"
	// OutcomeTimedOut is nobody answering inside the gate's window.
	OutcomeTimedOut Outcome = "timed_out"
	// OutcomeDismissed is the turn being cancelled or interrupted before an
	// answer arrived.
	OutcomeDismissed Outcome = "dismissed"
)

// Subject is the kind of thing an approval is about. It picks the voice of the
// card's subtitles: a gate asks about a tool an agent wants to use, while the
// scheduler asks about a held job whose first run has come due — where "the
// agent wants to" would simply be untrue, since a timer asked, not an agent.
type Subject string

const (
	// SubjectTool is a gated tool call. It is the zero value: every gate is one,
	// and a Spec that names no subject reads as one.
	SubjectTool Subject = ""
	// SubjectJob is a scheduled job's first run, held for the admin's approval.
	SubjectJob Subject = "job"
)

// Spec is what a card describes: the thing being gated and, optionally, the
// concrete thing it wants to do.
type Spec struct {
	// Subject is what kind of thing Name names. The zero value is a tool.
	Subject Subject
	// Name is the short label of the subject — the tool ("terminal", "edit") or
	// the job ("nightly-report"). Shown in the subtitle.
	Name string
	// Detail is the command line, prompt or tool title, rendered as a
	// preformatted block. Empty renders no code block at all — an approval for
	// something that carries no command should not show an empty box.
	Detail string
	// Language hints the preformatted block's syntax highlighting ("bash", or
	// empty for none).
	Language string
	// Note is an optional context line rendered under the detail while the card
	// is awaiting a decision — the scheduler puts the job's schedule there. The
	// settled card drops it: its context line reports the outcome, and two
	// stacked context lines on a collapsed card read as noise.
	Note string
}

// Option is one decision button, already addressed by the Broker: the action_id
// and value are the broker's, and the template must emit them verbatim or the
// click cannot be correlated back to the blocked call.
type Option struct {
	ActionID string
	Value    string
	Label    string
	Style    string // "", "primary", or "danger"
}

// Data is the context the templates render against. Field names are part of the
// template contract: renaming one breaks any operator override.
type Data struct {
	Title    string
	Subtitle string
	IconURL  string
	Detail   string
	Language string
	// ActionsBlockID is the block_id stamped on the button row. It is supplied
	// by the caller rather than fixed here because it is the gateway router's
	// constant, not this package's.
	ActionsBlockID string
	Options        []Option
	// Footer is the card's context line: on a resolved card the outcome and,
	// when known, who decided it — always non-empty there, so its child_blocks
	// never render empty; on a pending card the Spec's Note, which may be empty
	// and then renders no context block at all.
	Footer string
}

// Renderer turns the card templates into the raw Block Kit JSON the Slack
// client posts verbatim.
type Renderer struct {
	tpl *jsontemplate.Renderer
}

// NewRenderer builds a Renderer resolving templates from dir first, then fsys.
func NewRenderer(dir string, fsys fs.FS) *Renderer {
	return &Renderer{tpl: jsontemplate.New(dir, fsys)}
}

// Pending renders the card asking for a decision, with one button per option.
func (r *Renderer) Pending(spec Spec, actionsBlockID string, options []Option) ([]byte, error) {
	return r.render(PendingTemplate, Data{
		Title:          "Approval Needed",
		Subtitle:       pendingSubtitle(spec),
		IconURL:        iconPending,
		Detail:         strings.TrimRight(spec.Detail, "\n"),
		Language:       spec.Language,
		ActionsBlockID: actionsBlockID,
		Options:        options,
		Footer:         strings.TrimSpace(spec.Note),
	})
}

// Resolved renders the terminal card: no buttons, collapsed by default, and a
// context line saying what happened and who decided it. decidedBy is a Slack
// user id, or "" when nobody chose (a timeout or a dismissal).
func (r *Renderer) Resolved(spec Spec, outcome Outcome, decidedBy string) ([]byte, error) {
	icon := iconPending
	if outcome != OutcomeApproved {
		icon = iconRefused
	}
	return r.render(ResolvedTemplate, Data{
		Title:    resolvedTitle(outcome),
		Subtitle: resolvedSubtitle(spec, outcome),
		IconURL:  icon,
		Detail:   strings.TrimRight(spec.Detail, "\n"),
		Language: spec.Language,
		Footer:   footer(spec, outcome, decidedBy),
	})
}

func (r *Renderer) render(ref string, data Data) ([]byte, error) {
	out, err := r.tpl.Render(ref, data)
	if err != nil {
		return nil, fmt.Errorf("approvalcard: render %s: %w", ref, err)
	}
	return out, nil
}

// label is the subject as it reads inside a sentence, matching the canvas mock's
// quoting. An unnamed subject degrades to a neutral noun rather than an empty
// pair of quotes.
func label(spec Spec) string {
	name := strings.TrimSpace(spec.Name)
	if spec.Subject == SubjectJob {
		if name == "" {
			return "a scheduled job"
		}
		return "the '" + name + "' job"
	}
	if name == "" {
		return "a tool"
	}
	return "the '" + name + "' tool"
}

// pendingSubtitle says who is asking and for what. A job is its own actor: it
// came due on a schedule nobody re-triggered, so it is the subject of the
// sentence rather than something an agent reached for.
func pendingSubtitle(spec Spec) string {
	if spec.Subject == SubjectJob {
		return "Murtaugh is holding " + label(spec) + " before its first run"
	}
	return "The agent wants to use " + label(spec)
}

// resolvedTitle names the outcome in the header, where the pending card said
// "Approval Needed". Leaving the pending title in place on a settled card (as
// the canvas mock does) reads as though it were still waiting on someone.
func resolvedTitle(outcome Outcome) string {
	switch outcome {
	case OutcomeApproved:
		return "Approved"
	case OutcomeDenied:
		return "Denied"
	case OutcomeTimedOut:
		return "Approval Timed Out"
	default:
		return "Approval Dismissed"
	}
}

func resolvedSubtitle(spec Spec, outcome Outcome) string {
	subject := label(spec)
	if spec.Subject == SubjectJob {
		switch outcome {
		case OutcomeApproved:
			return "Murtaugh ran " + subject + ", and will run it on schedule from now on"
		case OutcomeDenied:
			return subject + " was not allowed to run"
		case OutcomeTimedOut:
			return "Nobody answered, so " + subject + " did not run"
		default:
			return "The request was dismissed, so " + subject + " did not run"
		}
	}
	switch outcome {
	case OutcomeApproved:
		return "The agent ran " + subject
	case OutcomeDenied:
		return "The agent was not allowed to use " + subject
	case OutcomeTimedOut:
		return "Nobody answered, so the agent did not use " + subject
	default:
		return "The request was dismissed, so the agent did not use " + subject
	}
}

// footer renders the context line under a resolved card. The decider is named
// here rather than in the subtitle because a subtitle is plain_text, which does
// not linkify a <@user> mention — and in a shared channel, who approved a
// command is the single most useful thing the settled card can carry.
func footer(spec Spec, outcome Outcome, decidedBy string) string {
	switch outcome {
	case OutcomeApproved:
		return "Approved" + by(decidedBy)
	case OutcomeDenied:
		return "Denied" + by(decidedBy)
	case OutcomeTimedOut:
		return "No response in time — " + label(spec) + " was not run."
	default:
		return "Dismissed before anyone answered — " + label(spec) + " was not run."
	}
}

func by(userID string) string {
	if strings.TrimSpace(userID) == "" {
		return "."
	}
	return fmt.Sprintf(" by <@%s>.", userID)
}

// FallbackText is the notification line shown where blocks cannot render — a
// push notification, a screen reader, the sidebar preview. It names the tool or
// job but never the command: a push notification is the least private surface
// Slack has.
func FallbackText(spec Spec) string {
	if strings.TrimSpace(spec.Name) == "" {
		return "Approval needed"
	}
	return "Approval needed for " + label(spec)
}
