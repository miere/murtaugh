package interaction

import (
	"context"
	"fmt"
	"strings"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/slack/approvalcard"
)

// PermissionGate answers an ACP agent's session/request_permission by posting the
// agent's offered options as buttons in the turn's Slack thread and returning the
// chosen optionId. It satisfies agent.PermissionAsker structurally, so the agent
// package stays free of any Slack dependency. It is the ACP analogue of
// GateApprover (which gates the native loop's tool calls) over the same broker.
type PermissionGate struct {
	broker *Broker
	// cards renders the approval container card. nil falls back to the broker's
	// plain button-row rendering, which is what the tests exercise.
	cards *approvalcard.Renderer
	// keepResolved leaves the settled card in the conversation instead of
	// deleting it after the broker's TTL. It is this agent's
	// approval.keep_resolved, which is why a gate is built per agent.
	keepResolved bool
	// grants is the always-allow set, shared with this agent's GateApprover. It is
	// only ever *read* here: the options on this path belong to the agent, so this
	// gate offers no way to create a grant — see AskPermission.
	grants *Grants
}

// NewPermissionGate builds a PermissionGate over the shared broker, rendering
// with cards, honouring the agent's keep_resolved setting, and honouring the
// always-allow choices recorded in grants (shared with the agent's approver).
func NewPermissionGate(broker *Broker, cards *approvalcard.Renderer, keepResolved bool, grants *Grants) *PermissionGate {
	return &PermissionGate{broker: broker, cards: cards, keepResolved: keepResolved, grants: grants}
}

// AskPermission posts the agent's offered options as buttons in loc's thread and
// blocks until the user picks one (or the wait times out / is cancelled). It
// returns the chosen option's ID, or "" when the user did not choose — which the
// ACP client maps to a "cancelled" outcome. With no broker or no Slack location it
// returns "" (deny), leaving the caller to fail fast rather than block.
func (g *PermissionGate) AskPermission(ctx context.Context, loc agent.TurnLocation, req agent.PermissionRequest) (string, error) {
	if g == nil || g.broker == nil || loc.ChannelID == "" {
		return "", nil
	}
	options := make([]Option, 0, len(req.Options))
	kindByID := make(map[string]string, len(req.Options))
	for _, o := range req.Options {
		label := o.Name
		if label == "" {
			label = o.ID
		}
		options = append(options, Option{ID: o.ID, Label: label, Style: styleForPermissionKind(o.Kind)})
		kindByID[o.ID] = o.Kind
	}
	if len(options) == 0 {
		return "", nil
	}
	// The options above are the agent's, copied through untouched — Murtaugh adds
	// none of its own here. On this path the agent owns the vocabulary: it will
	// only understand an optionId it declared, and an agent that already offers
	// allow_always is doing its own bookkeeping, so a second Murtaugh-flavoured
	// button would be both unroutable and a lie about who is remembering what.
	//
	// What Murtaugh can do is decline to ask a question already answered: if this
	// call matches a grant the user made through Murtaugh's own gate, answer it
	// with the agent's own allow option and post nothing.
	name := friendlyToolName(req.ToolKind)
	detail := strings.TrimRight(req.ToolTitle, "\n")
	if id := allowOptionID(req.Options); id != "" && g.grants.Allowed(GrantKey(name, detail)) {
		return id, nil
	}
	// Mirror the native approval gate: name the tool concisely and, when the agent
	// supplied a title (for an execute call, the command line), render it in a
	// language-hinted fenced code block via Slack's markdown block — the same
	// syntax-highlighted treatment the agent's own code output gets — rather than
	// echoing the whole command inline.
	question := fmt.Sprintf("The agent wants to use the `%s` tool. Allow?", name)
	if detail != "" {
		question = fmt.Sprintf("The agent wants to use the `%s` tool:\n\n```%s\n%s\n```\n\nAllow?", name, codeLang(name), detail)
	}
	decision, err := g.broker.Ask(ctx, Destination{ChannelID: loc.ChannelID, ThreadTS: loc.ThreadTS}, PromptSpec{
		Title:       ":lock: Permission needed",
		Question:    question,
		Markdown:    true,
		Options:     options,
		OutcomeText: permissionOutcome(name, kindByID),
		// The settled card is deleted after the broker's TTL unless this agent
		// asked to keep it.
		AutoDismiss: !g.keepResolved,
		Cards:       g.approvalCards(name, detail, kindByID),
	})
	if err != nil {
		return "", err
	}
	if decision.Answered() {
		return decision.OptionID, nil
	}
	return "", nil
}

// approvalCards builds the renderer hook for one permission request, or nil when
// this gate has no card renderer (the plain-prompt path the tests use).
func (g *PermissionGate) approvalCards(name, detail string, kindByID map[string]string) CardRenderer {
	if g.cards == nil {
		return nil
	}
	return approvalCards{
		cards: g.cards,
		spec: approvalcard.Spec{
			ToolName: name,
			Detail:   detail,
			Language: codeLang(name),
		},
		outcome: permissionCardOutcome(kindByID),
	}
}

// allowOptionID picks the option to answer a pre-granted call with, preferring
// allow_once over allow_always. The preference is deliberate and matches what
// the ACP client does when auto-allowing: a grant recorded on Murtaugh's side is
// Murtaugh's to remember, and answering with allow_always would additionally ask
// the agent to remember it — escalating a local grant into a standing permission
// on the far side of a boundary Murtaugh does not control.
//
// It returns "" when the agent offered no allow option at all, which leaves the
// caller to ask the human as usual rather than inventing an answer.
func allowOptionID(options []agent.PermissionOption) string {
	var fallback string
	for _, o := range options {
		if !strings.HasPrefix(o.Kind, "allow") {
			continue
		}
		if o.Kind == "allow_once" {
			return o.ID
		}
		if fallback == "" {
			fallback = o.ID
		}
	}
	return fallback
}

// permissionOutcome renders the terminal line an ACP permission prompt is
// rewritten to, mirroring the native approval gate: an allow_* choice shows a
// check, a reject_* choice is struck through, and both name the decider. The
// option kinds are agent-defined, so an unrecognised kind falls back to naming
// the chosen option plainly rather than guessing allow vs deny.
func permissionOutcome(toolName string, kindByID map[string]string) func(Decision) string {
	return func(d Decision) string {
		switch {
		case d.TimedOut:
			return fmt.Sprintf(":hourglass_flowing_sand: Permission for `%s` timed out", toolName)
		case d.Cancelled:
			return fmt.Sprintf(":no_entry_sign: Permission for `%s` dismissed", toolName)
		}
		kind := kindByID[d.OptionID]
		switch {
		case strings.HasPrefix(kind, "allow"):
			return fmt.Sprintf("✓ Tool `%s` approved%s", toolName, decidedBy(d.UserID))
		case strings.HasPrefix(kind, "reject"):
			return fmt.Sprintf("~Tool `%s` denied%s~", toolName, decidedBy(d.UserID))
		default:
			label := d.Label
			if label == "" {
				label = d.OptionID
			}
			return fmt.Sprintf("✓ Tool `%s`: *%s*%s", toolName, label, decidedBy(d.UserID))
		}
	}
}

// friendlyToolName maps an ACP toolCall kind to the short, stable label shown in
// the permission prompt and its outcome line. The execute kind is surfaced as
// "terminal" so the ACP gate reads identically to the native one (whose tool is
// literally named "terminal", and which codeLang keys on for bash highlighting);
// other known kinds use the kind verbatim, and an empty/unknown kind falls back to
// a neutral "tool".
func friendlyToolName(kind string) string {
	switch k := strings.ToLower(strings.TrimSpace(kind)); k {
	case "":
		return "tool"
	case "execute":
		return "terminal"
	default:
		return k
	}
}

// styleForPermissionKind maps an ACP PermissionOptionKind to a button style:
// allow_* renders primary (green), reject_* danger (red), unknown neutral.
func styleForPermissionKind(kind string) string {
	switch {
	case strings.HasPrefix(kind, "allow"):
		return "primary"
	case strings.HasPrefix(kind, "reject"):
		return "danger"
	default:
		return ""
	}
}
