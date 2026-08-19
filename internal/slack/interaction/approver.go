package interaction

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/slack/approvalcard"
)

// GateApprover is the Slack-backed approval gate: it asks the user to confirm a
// side-effecting tool call via the broker's Approve/Deny prompt, in the turn's
// own conversation. It satisfies the native loop's Approver interface
// structurally, so the native package stays free of any Slack dependency.
//
// It also keeps an in-memory "always allow" set: when the user picks
// "Approve & always allow", the exact summary string is remembered and every
// later call with the same summary is approved silently, without re-prompting.
// The set is session-scoped — it lives on the GateApprover and resets when the
// daemon restarts; nothing is persisted to config. Matching is exact (after
// trimming surrounding whitespace), with no fuzzy/normalizing comparison.
type GateApprover struct {
	broker *Broker
	// cards renders the approval container card. nil falls back to the broker's
	// plain button-row rendering, which is what the tests exercise.
	cards *approvalcard.Renderer
	// keepResolved leaves the settled card in the conversation instead of
	// deleting it after the broker's TTL. It is this agent's
	// approval.keep_resolved, which is why an approver is built per agent.
	keepResolved bool

	mu      sync.Mutex
	allowed map[string]bool // summaries the user chose to always allow this run
}

// NewApprover builds a GateApprover over the shared broker, rendering with cards
// and honouring the agent's keep_resolved setting.
func NewApprover(broker *Broker, cards *approvalcard.Renderer, keepResolved bool) *GateApprover {
	return &GateApprover{
		broker:       broker,
		cards:        cards,
		keepResolved: keepResolved,
		allowed:      make(map[string]bool),
	}
}

// Approve asks the user to confirm running toolName with the given summary,
// posting Approve / Approve & always allow / Deny buttons into the current Slack
// thread and blocking until they answer. It returns whether to run the tool and,
// when not, a note for the model.
//
// If the (trimmed) summary was previously marked "always allow" this run, the
// call is approved immediately with no prompt. The always-allow set is
// session-scoped and matched exactly on the summary string (for the terminal
// tool, that is the command line).
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

	key := strings.TrimSpace(summary)
	if g.isAllowed(key) {
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
		g.remember(key)
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
			ToolName: toolName,
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

// isAllowed reports whether key is in the session-scoped always-allow set.
func (g *GateApprover) isAllowed(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.allowed[key]
}

// remember adds key to the always-allow set for the rest of this run.
func (g *GateApprover) remember(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.allowed[key] = true
}
