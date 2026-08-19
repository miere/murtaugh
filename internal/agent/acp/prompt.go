package acp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/miere/murtaugh/internal/agent"
)

// promptBlocks renders a agent.PromptRequest into ACP `session/prompt` content
// blocks. ACP exposes no system role, so leading delimited blocks are the
// closest stand-in for a system note. Order:
//  0. a <persona> block (only when a shared persona is configured) carrying
//     Murtaugh's voice, so an ACP agent reads as the same character as native
//     even when it runs in an external project with its own AGENTS.md.
//  1. a <context> block carrying the volatile per-turn facts (current time,
//     working directory) — the ACP analogue of native's RenderTurnContext, so
//     an ACP agent knows what day it is and where it is rooted, just like the
//     native loop. Emitted for every caller, chat or CLI.
//  2. a <conversation-context> block (only when the prompt carries a Slack
//     conversation) telling the agent where it is talking so it can hand the
//     same channel/thread to the `restart` tool. Kept as a separate block with
//     machine-readable channel/thread attributes so that parseability is
//     unchanged.
//  3. the thread transcript, when History is set (a freshly opened session
//     backfilling an existing thread).
//  4. the user's text.
func (c *acpSession) promptBlocks(request agent.PromptRequest) []map[string]string {
	blocks := make([]map[string]string, 0, 5)
	if persona := strings.TrimSpace(c.opts.Persona); persona != "" {
		blocks = append(blocks, map[string]string{"type": "text", "text": "<persona>\n" + persona + "\n</persona>"})
	}
	if ctxText := c.renderTurnContext(); ctxText != "" {
		blocks = append(blocks, map[string]string{"type": "text", "text": ctxText})
	}
	if request.Channel != "" {
		ctxText := fmt.Sprintf(
			"<conversation-context channel=%q thread=%q>You are responding in this Slack conversation. "+
				"If you call the `restart` tool, pass these exact channel and thread values so the approval "+
				"card is asked here.</conversation-context>",
			request.Channel, request.Thread,
		)
		blocks = append(blocks, map[string]string{"type": "text", "text": ctxText})
	}
	if request.History != "" {
		blocks = append(blocks, map[string]string{"type": "text", "text": request.History})
	}
	blocks = append(blocks, map[string]string{"type": "text", "text": request.Text})
	return blocks
}

// renderTurnContext renders the volatile per-turn <context> block (current time
// and working directory) for an ACP prompt, or "" when there is nothing to say.
// It mirrors the native RenderTurnContext format so the two backends present the
// same facts to the model; the Slack location is intentionally left to the
// separate <conversation-context> block above.
func (c *acpSession) renderTurnContext() string {
	var lines []string
	if c.now != nil {
		if now := c.now(); !now.IsZero() {
			lines = append(lines, "It is currently "+now.Format("2006-01-02 15:04 MST"))
		}
	}
	if cwd := c.sessionCWD(); cwd != "" && cwd != "." {
		lines = append(lines, "Working directory: "+cwd)
	}
	if len(lines) == 0 {
		return ""
	}
	return "<context>\n" + strings.Join(lines, "\n") + "\n</context>"
}

// prompt drives one turn: it installs the turn's subscription/scope/watcher,
// starts the heartbeat, sends session/prompt, and streams events until the turn
// completes. The returned channel closes when the turn ends.
func (c *acpSession) prompt(ctx context.Context, request agent.PromptRequest) (<-chan agent.Event, error) {
	sub := &subscription{events: make(chan agent.Event, 32)}
	events := sub.events
	sawText := &atomic.Bool{}
	watcher := agent.NewToolWatcher(c.now)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("ACP session is closed")
	}
	sessionID := c.sessionID
	c.active = sub
	c.scope = promptScope{loc: agent.TurnLocation{ChannelID: request.Channel, ThreadTS: request.Thread}, ctx: ctx, sawText: sawText}
	c.watcher = watcher
	c.mu.Unlock()

	go func() {
		// The heartbeat keeps this turn alive while a tool legitimately runs and
		// fails it if one runs past its ceiling. It rides an inner cancellable
		// context so the ceiling can unblock the in-flight session/prompt without
		// disturbing the caller's ctx.
		promptCtx, cancelPrompt := context.WithCancelCause(ctx)
		defer cancelPrompt(nil)
		stopHB := make(chan struct{})
		hbDone := make(chan struct{})
		go c.heartbeat(promptCtx, watcher, events, cancelPrompt, stopHB, hbDone)
		defer func() {
			close(stopHB)
			<-hbDone
			c.clearToolWatch(watcher)
			c.closeSubscription(sub)
		}()
		result, err := c.call(promptCtx, "session/prompt", map[string]any{
			"sessionId": sessionID,
			"prompt":    c.promptBlocks(request),
		})
		if err != nil {
			if cause := context.Cause(promptCtx); errors.Is(cause, ErrToolCeiling) {
				events <- agent.Event{Type: agent.EventError, Error: cause}
				return
			}
			events <- agent.Event{Type: agent.EventError, Error: err}
			return
		}
		text := extractText(result)
		stopReason := extractStopReason(result)
		c.log.Info("ACP prompt completed", "session_id", sessionID, "stop_reason", stopReason, "response_text", text != "")
		// Emit the final result text ONLY when nothing was streamed this turn.
		// Streaming agents deliver the reply incrementally via agent_message_chunk
		// notifications and echo the same text in the prompt result; re-emitting it
		// here would duplicate the whole reply. A non-streaming agent still has its
		// reply surfaced, since nothing set sawText.
		if text != "" && !sawText.Load() {
			events <- agent.Event{Type: agent.EventText, Text: text}
		}
		// A cancelled turn is not a completion. The caller interrupted it, and the
		// result it left behind carries no reply — so completing it would hand the
		// relay "the agent finished and said nothing" and earn the user a warning
		// about an agent that did as it was told. Raised as a cancellation instead,
		// exactly as the claudecode backend does with its aborted-turn result, so
		// both backends render the same interrupt marker.
		if isCancelledStopReason(stopReason) {
			events <- agent.Event{Type: agent.EventError, Error: fmt.Errorf("acp: turn interrupted: %w", context.Canceled)}
			return
		}
		events <- agent.Event{Type: agent.EventComplete, StopReason: stopReason}
	}()
	return events, nil
}

// closeSubscription retracts this turn's subscription and closes its event
// channel — but only after every readLoop-originated send that already captured
// the subscription has drained. The readLoop registers as an in-flight sender
// (sub.wg.Add) under mu before sending; retracting active under mu stops NEW
// captures, and wg.Wait waits out the ones already past the lookup. Closing before
// that drain is what let a trailing notification panic on a closed channel.
func (c *acpSession) closeSubscription(sub *subscription) {
	c.mu.Lock()
	if c.active == sub {
		c.active = nil
		c.scope = promptScope{}
	}
	c.mu.Unlock()
	sub.wg.Wait()
	close(sub.events)
}

// clearToolWatch retracts this turn's tool watcher if it is still the live one.
func (c *acpSession) clearToolWatch(w *agent.ToolWatcher) {
	c.mu.Lock()
	if c.watcher == w {
		c.watcher = nil
	}
	c.mu.Unlock()
}
