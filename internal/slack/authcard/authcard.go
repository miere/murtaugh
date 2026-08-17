// Package authcard renders and routes the two-party authentication card.
//
// An auth request involves two people. The agent's turn is running for whoever
// asked it to do something, but the credentials belong to the admin — so the
// requester gets a partial notice in their thread ("your admin has been
// notified") and the admin gets the card that actually does the work, in their
// DM. One lifecycle drives both: whatever the admin does, or fails to do,
// updates the requester's card and decides the tool's result.
//
// The cards are Block Kit JSON templates under assets/templates/auth, rendered
// through internal/jsontemplate and posted verbatim via the client's raw-blocks
// passthrough. They use block types (container, rich_text) newer than the
// pinned slack-go, which is exactly why they are templates rather than Go
// builders.
//
// Correlation works like the interaction broker's: a random id minted per
// request, carried in the buttons' action_id namespace, recognised by the
// gateway router and handed back here.
package authcard

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
	// BlockID tags the admin card's actions block. The gateway router
	// recognises an auth interaction by it or by the action_id prefix.
	BlockID = "murtaugh_auth"

	// ActionPrefix namespaces every action_id the admin card emits. The
	// correlation id and the action name are appended:
	// "murtaugh_auth:<corr>:<action>".
	ActionPrefix = "murtaugh_auth:"

	// ModalCallbackID is the callback_id on the verification-code modal. The
	// correlation id rides in PrivateMetadata, not here.
	ModalCallbackID = "murtaugh_auth_code_modal"

	// codeBlockID / codeActionID identify the modal's single text input, so the
	// submitted value can be found in view.State.Values.
	codeBlockID  = "murtaugh_auth_code_block"
	codeActionID = "murtaugh_auth_code_input"
)

// Template references, resolved against the config dir first and the embedded
// assets tree second — so an operator can restyle a card without a rebuild.
const (
	RequesterTemplate = "templates/auth/requester.json"
	AdminTemplate     = "templates/auth/admin.json"
)

// Action is one button on the admin card.
type Action string

const (
	// ActionPrimary is the single-attempt action: "Enter Code" on a code flow,
	// "Open In Browser" on a browser-only one. Clicking it retires the whole
	// actions bar.
	ActionPrimary Action = "primary"
	// ActionOpen is the secondary "Open In Browser" link shown only on a code
	// flow, where the user must visit the URL *before* they have a code to
	// paste. It deliberately does not retire the bar.
	ActionOpen Action = "open"
	// ActionDeny refuses the request outright.
	ActionDeny Action = "deny"
)

// State is what the pair of cards is currently showing.
type State string

const (
	StatePending State = "pending" // posted, waiting on the admin
	StateWorking State = "working" // primary clicked; buttons retired, waiting on completion
	StateSuccess State = "success"
	StateDenied  State = "denied"
	StateTimeout State = "timeout"
	StateFailed  State = "failed"
)

// Terminal reports whether s is an end state, after which neither card changes
// again.
func (s State) Terminal() bool {
	switch s {
	case StateSuccess, StateDenied, StateTimeout, StateFailed:
		return true
	default:
		return false
	}
}

// ActionID builds the action_id for one button of one request.
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

// IsAuthInteraction reports whether ic is a click on an auth card, returning the
// correlation id and which button. The gateway uses it to dispatch before the
// workflow engine sees the callback.
func IsAuthInteraction(ic slackgo.InteractionCallback) (corr string, action Action, ok bool) {
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

// ParseCodeSubmission reads a verification code out of the modal's
// view_submission callback. ok is false when this is not our modal.
func ParseCodeSubmission(ic slackgo.InteractionCallback) (corr, code string, ok bool) {
	if ic.Type != slackgo.InteractionTypeViewSubmission {
		return "", "", false
	}
	if ic.View.CallbackID != ModalCallbackID {
		return "", "", false
	}
	corr = ic.View.PrivateMetadata
	if ic.View.State == nil {
		return corr, "", true
	}
	for _, actions := range ic.View.State.Values {
		for _, action := range actions {
			if v := strings.TrimSpace(action.Value); v != "" {
				return corr, v, true
			}
		}
	}
	return corr, "", true
}

// CodeModal builds the verification-code prompt opened by the primary button on
// a code flow. The correlation id rides in PrivateMetadata so the submission can
// be routed back to the waiting request.
//
// Modals are still built with slack-go's typed request rather than a JSON
// template: OpenView takes a typed ModalViewRequest, and views.open is a
// different API surface from the raw-blocks message path the cards use.
func CodeModal(corr, toolName string) slackgo.ModalViewRequest {
	label := "Verification code"
	input := slackgo.NewPlainTextInputBlockElement(
		slackgo.NewTextBlockObject(slackgo.PlainTextType, "Paste the code from your browser", false, false),
		codeActionID,
	)
	hint := strings.TrimSpace(toolName)
	if hint == "" {
		hint = "the requesting tool"
	}
	return slackgo.ModalViewRequest{
		Type:   slackgo.VTModal,
		Title:  slackgo.NewTextBlockObject(slackgo.PlainTextType, "Authentication", false, false),
		Submit: slackgo.NewTextBlockObject(slackgo.PlainTextType, "Submit", false, false),
		Close:  slackgo.NewTextBlockObject(slackgo.PlainTextType, "Cancel", false, false),
		Blocks: slackgo.Blocks{BlockSet: []slackgo.Block{
			slackgo.NewInputBlock(
				codeBlockID,
				slackgo.NewTextBlockObject(slackgo.PlainTextType, label, false, false),
				slackgo.NewTextBlockObject(slackgo.PlainTextType, "Finishes authentication for "+clamp(hint, 100), false, false),
				input,
			),
		}},
		CallbackID:      ModalCallbackID,
		PrivateMetadata: corr,
	}
}

// cardData is the context the card templates render against. Field names are
// part of the template contract: renaming one breaks any operator override.
type cardData struct {
	ToolName        string
	ProfileName     string
	URL             string
	NeedsCode       bool
	RequesterUserID string
	AttemptAt       string
	State           string
	Reason          string

	ShowActions   bool
	ShowFooter    bool
	ShowRequester bool

	ActionPrimary string
	ActionOpen    string
	ActionDeny    string
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

func (r *Renderer) render(ref string, data cardData) ([]byte, error) {
	out, err := r.tpl.Render(ref, data)
	if err != nil {
		return nil, fmt.Errorf("authcard: render %s: %w", ref, err)
	}
	return out, nil
}

// attemptFormat matches the example card: "May 14, 2026 at 3:42 PM".
const attemptFormat = "Jan 2, 2006 at 3:04 PM"

// fallbackText is the notification line shown where blocks cannot render. It
// names the tool but nothing else — a push notification is the least private
// surface Slack has.
func fallbackText(toolName string) string {
	name := strings.TrimSpace(toolName)
	if name == "" {
		return "Authentication required"
	}
	return "Authentication required for " + clamp(name, 100)
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
		return "", fmt.Errorf("authcard: mint correlation id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// nowFunc is the clock, injectable so tests get a stable AttemptAt.
type nowFunc func() time.Time
