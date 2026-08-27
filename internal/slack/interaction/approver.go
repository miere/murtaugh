package interaction

import (
	"context"
	"fmt"
	"strings"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/slack/approvalcard"
)

// GateApprover is the Slack-backed approval gate: it asks the user to confirm a
// side-effecting tool call via the broker's Approve/Deny prompt, in the turn's
// own conversation. It satisfies the native loop's Approver interface
// structurally, so the native package stays free of any Slack dependency.
//
// When the user picks "Approve & always allow", the call is recorded in the
// agent's shared Grants set and every later call with the same key is approved
// silently, without re-prompting. The set is shared with the agent's
// PermissionGate, so a command allowed here is not asked about again when the
// agent runs it through its own harness. See Grants for the scope and matching
// rules.
type GateApprover struct {
	broker *Broker
	// cards renders the approval container card. nil falls back to the broker's
	// plain button-row rendering, which is what the tests exercise.
	cards *approvalcard.Renderer
	// keepResolved leaves the settled card in the conversation instead of
	// deleting it after the broker's TTL. It is this agent's
	// approval.keep_resolved, which is why an approver is built per agent.
	keepResolved bool
	// grants is the always-allow set, shared with this agent's PermissionGate so
	// a grant made here is honoured when the agent asks about the same call
	// through its own harness. nil disables always-allow entirely.
	grants *Grants
}

// NewApprover builds a GateApprover over the shared broker, rendering with cards,
// honouring the agent's keep_resolved setting, and recording always-allow
// choices in grants (shared with the agent's permission gate).
func NewApprover(broker *Broker, cards *approvalcard.Renderer, keepResolved bool, grants *Grants) *GateApprover {
	return &GateApprover{
		broker:       broker,
		cards:        cards,
		keepResolved: keepResolved,
		grants:       grants,
	}
}

// Approve asks the user to confirm running toolName with the given summary,
// posting Approve / Approve & always allow / Deny buttons into the current Slack
// thread and blocking until they answer. It returns whether to run the tool and,
// when not, a note for the model.
//
// If this call was previously marked "always allow" this run — here or on the
// agent's own permission path, which shares the set — it is approved immediately
// with no prompt. See GrantKey for how a call is identified.
//
// When there is no Slack conversation on the context (a headless/delegated run),
// the call is NOT gated — the run was arranged without a human to ask, so it
// proceeds. The gate exists to catch eager behaviour in live chat; that is the
// only place a TurnLocation is set.
func (g *GateApprover) Approve(ctx context.Context, toolName, summary string) (bool, string) {
	if g == nil || g.broker == nil {
		return true, ""
	}
	loc, ok := agent.TurnLocationFromContext(ctx)
	if !ok {
		return true, ""
	}

	key := GrantKey(toolName, summary)
	if g.grants.Allowed(key) {
		return true, ""
	}

	// The summary (for the terminal tool, the command line) goes in a language-
	// hinted fenced code block and is rendered via Slack's markdown block, so it
	// gets the same syntax highlighting as the agent's own code output rather than
	// a flat monospace span.
	question := fmt.Sprintf("The agent wants to run the `%s` tool:\n\n```%s\n%s\n```\n\nApprove?", toolName, codeLang(toolName), strings.TrimRight(summary, "\n"))
	decision, err := g.broker.Ask(ctx, Destination{ChannelID: loc.ChannelID, ThreadTS: loc.ThreadTS}, PromptSpec{
		Title:    ":lock: Approval needed",
		Question: question,
		Markdown: true,
		Options: []Option{
			{ID: "approve", Label: "Approve", Style: "primary"},
			{ID: "approve_always", Label: "Approve & always allow", Style: "primary"},
			{ID: "deny", Label: "Deny", Style: "danger"},
		},
		OutcomeText: approvalOutcome(toolName),
		// The settled card is deleted after the broker's TTL unless this agent
		// asked to keep it.
		AutoDismiss: !g.keepResolved,
		Cards:       g.approvalCards(toolName, summary),
	})
	if err != nil {
		return false, fmt.Sprintf("Skipped: couldn't ask for approval (%v). Not run.", err)
	}
	switch {
	case decision.OptionID == "approve_always":
		g.grants.Remember(key)
		return true, ""
	case decision.OptionID == "approve":
		return true, ""
	case decision.TimedOut:
		return false, "Skipped: no approval received in time. The action was not run — ask again if it is still needed."
	case decision.Cancelled:
		return false, "Skipped: the approval request was dismissed. The action was not run."
	default:
		return false, "Denied by the user. The action was not run; do not retry it without their go-ahead."
	}
}

// approvalOutcome renders the terminal line the approval prompt is rewritten to,
// keyed by the decision. It is deliberately concise — naming the tool and the
// decider — rather than echoing the (code-laden) question. A denial is struck
// through; a timeout/dismissal is reported plainly without a decider.
func approvalOutcome(toolName string) func(Decision) string {
	return func(d Decision) string {
		switch {
		case d.TimedOut:
			return fmt.Sprintf(":hourglass_flowing_sand: Tool `%s` approval timed out", toolName)
		case d.Cancelled:
			return fmt.Sprintf(":no_entry_sign: Tool `%s` approval dismissed", toolName)
		case d.OptionID == "deny":
			return fmt.Sprintf("~Tool `%s` denied%s~", toolName, decidedBy(d.UserID))
		default: // approve / approve_always
			return fmt.Sprintf("✓ Tool `%s` approved%s", toolName, decidedBy(d.UserID))
		}
	}
}

// approvalCards builds the renderer hook for one gated call, or nil when this
// approver has no card renderer (the plain-prompt path the tests use).
func (g *GateApprover) approvalCards(toolName, summary string) CardRenderer {
	if g.cards == nil {
		return nil
	}
	return approvalCards{
		cards: g.cards,
		spec: approvalcard.Spec{
			Name:     toolName,
			Detail:   strings.TrimRight(summary, "\n"),
			Language: codeLang(toolName),
		},
		outcome: nativeOutcome,
	}
}

// codeLang picks the syntax-highlighting language for a tool's summary, used for
// both the fenced code block of the plain prompt and the card's preformatted
// block. A shell-running tool's summary is a command line; every other tool gets
// no hint (a plain, un-highlighted block).
func codeLang(toolName string) string {
	if isShellTool(toolName) {
		return "bash"
	}
	return ""
}

// isShellTool reports whether toolName runs a shell command line. Murtaugh's own
// native tool is called "terminal" and ACP calls the kind "execute", but a
// backend is free to name it whatever it likes — Claude Code's is literally
// "bash" — and a command line that loses its highlighting because of the name it
// arrived under is a display bug, not a policy decision. Matching is
// case-insensitive: the ACP kind is lowercased upstream, but a tool name from a
// backend's own vocabulary is not.
func isShellTool(toolName string) bool {
	switch strings.ToLower(strings.TrimSpace(toolName)) {
	case "terminal", "bash", "execute", "shell":
		return true
	default:
		return false
	}
}

// decidedBy renders the " by <@user>" suffix, or "" when the user is unknown.
func decidedBy(userID string) string {
	if userID == "" {
		return ""
	}
	return fmt.Sprintf(" by <@%s>", userID)
}
