package gateway

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/slack-go/slack"
)

// defaultCoalesceWindow is how long a conversation's incoming messages are
// batched before the turn is dispatched. It trades a little first-response
// latency for the ability to fold a rapid-fire burst ("do A", "also B") into
// one turn. Tunable; a smaller value is snappier but coalesces fewer messages.
const defaultCoalesceWindow = 500 * time.Millisecond

// stopper is the slice of *time.Timer the coalescer relies on. An interface so
// tests can substitute a manually-fired timer for deterministic scheduling.
type stopper interface{ Stop() bool }

// timerFunc schedules f to run after d and returns a handle to cancel it.
// Production wiring is time.AfterFunc; tests substitute a manual trigger.
type timerFunc func(d time.Duration, f func()) stopper

// coalescer serialises a conversation's turns and merges messages that arrive
// close together — or while a turn is already running — into a single coalesced
// turn. It replaces the older interrupt-and-replace behaviour. Per conversation
// key:
//
//   - messages arriving within a debounce window are batched into one turn;
//   - a message that lands while a turn is running is held until the debounce
//     fires; then an interruptible agent's turn is cancelled so the batch runs
//     now, while a non-interruptible agent's turn is left to finish first;
//   - on any turn's completion the pending batch (if any) is dispatched as one
//     coalesced turn.
//
// Net effect: rapid "do A", "also B", "and C" reach the agent as one prompt
// with full context, and no message is ever dropped — the improvement over
// interrupt-and-replace (which abandoned the earlier message) and over the
// non-interruptible drop path (which discarded the follow-up entirely).
type coalescer struct {
	debounce       time.Duration
	after          timerFunc
	interruptible  func(agentName string) bool
	cancelInFlight func(key agent.ConversationKey) bool
	dispatch       func(parent context.Context, key agent.ConversationKey, agentName string, route ChatRoute, req ChatRequest)
	logger         *slog.Logger

	mu      sync.Mutex
	pending map[agent.ConversationKey]*pendingBatch
	running map[agent.ConversationKey]bool
}

// pendingBatch accumulates the not-yet-dispatched messages for one conversation.
// parent/agentName/route track the most recent message so the coalesced turn
// carries fresh routing and correlation context.
type pendingBatch struct {
	parent    context.Context
	msgs      []ChatRequest
	agentName string
	route     ChatRoute
	timer     stopper
}

func newCoalescer(
	debounce time.Duration,
	after timerFunc,
	interruptible func(string) bool,
	cancelInFlight func(agent.ConversationKey) bool,
	dispatch func(context.Context, agent.ConversationKey, string, ChatRoute, ChatRequest),
	logger *slog.Logger,
) *coalescer {
	if debounce <= 0 {
		debounce = defaultCoalesceWindow
	}
	if after == nil {
		after = func(d time.Duration, f func()) stopper { return time.AfterFunc(d, f) }
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &coalescer{
		debounce:       debounce,
		after:          after,
		interruptible:  interruptible,
		cancelInFlight: cancelInFlight,
		dispatch:       dispatch,
		logger:         logger,
		pending:        make(map[agent.ConversationKey]*pendingBatch),
		running:        make(map[agent.ConversationKey]bool),
	}
}

// submit records a new message for the conversation and arms the debounce timer
// if one is not already running for this batch.
func (c *coalescer) submit(parent context.Context, key agent.ConversationKey, agentName string, route ChatRoute, req ChatRequest) {
	if c == nil {
		return
	}
	c.mu.Lock()
	b := c.pending[key]
	if b == nil {
		b = &pendingBatch{}
		c.pending[key] = b
	}
	b.parent = parent
	b.agentName = agentName
	b.route = route
	b.msgs = append(b.msgs, req)
	if b.timer == nil {
		b.timer = c.after(c.debounce, func() { c.onDebounce(key) })
	}
	c.mu.Unlock()
}

// onDebounce fires when a batch's window elapses. If nothing is running it
// dispatches the batch now; if a turn is running it either cancels it (the
// agent is interruptible, so the batch runs immediately after) or waits for the
// running turn to finish (non-interruptible), at which point onComplete drains.
func (c *coalescer) onDebounce(key agent.ConversationKey) {
	c.mu.Lock()
	b := c.pending[key]
	if b == nil {
		c.mu.Unlock()
		return
	}
	b.timer = nil
	if c.running[key] {
		interruptible := c.interruptible(b.agentName)
		c.mu.Unlock()
		if interruptible {
			// Cancel the in-flight turn; its completion (onComplete) drains the
			// pending batch and dispatches it as one coalesced turn.
			c.cancelInFlight(key)
		}
		// Non-interruptible: leave the batch in place; onComplete will drain it
		// when the running turn finishes on its own.
		return
	}
	parent, agentName, route, req, ok := c.takeLocked(key)
	if !ok {
		c.mu.Unlock()
		return
	}
	c.running[key] = true
	c.mu.Unlock()
	c.dispatch(parent, key, agentName, route, req)
}

// onComplete is called when a dispatched turn finishes (naturally, interrupted,
// or /stopped). It clears the running flag and, if messages queued during the
// turn, immediately dispatches them as one coalesced follow-up.
func (c *coalescer) onComplete(key agent.ConversationKey) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.running[key] = false
	parent, agentName, route, req, ok := c.takeLocked(key)
	if !ok {
		delete(c.running, key)
		c.mu.Unlock()
		return
	}
	c.running[key] = true
	c.mu.Unlock()
	c.dispatch(parent, key, agentName, route, req)
}

// clear drops any pending batch for the conversation without dispatching it.
// Used by /stop so queued messages do not run after the user cancels.
func (c *coalescer) clear(key agent.ConversationKey) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if b := c.pending[key]; b != nil && b.timer != nil {
		b.timer.Stop()
	}
	delete(c.pending, key)
	c.mu.Unlock()
}

// takeLocked removes the pending batch for key and returns it coalesced into a
// single request. The caller must hold c.mu. ok is false when nothing pending.
func (c *coalescer) takeLocked(key agent.ConversationKey) (context.Context, string, ChatRoute, ChatRequest, bool) {
	b := c.pending[key]
	if b == nil || len(b.msgs) == 0 {
		return nil, "", ChatRoute{}, ChatRequest{}, false
	}
	if b.timer != nil {
		b.timer.Stop()
	}
	delete(c.pending, key)
	return b.parent, b.agentName, b.route, coalesce(b.msgs), true
}

// coalesce merges a batch of same-conversation messages into one request. A lone
// message is returned unchanged (the overwhelming common case, so a single
// message is never reformatted). Several are joined in arrival order with a
// blank line between and their attachments concatenated; the last message is
// the base, so the reply anchors to the most recent one.
func coalesce(msgs []ChatRequest) ChatRequest {
	if len(msgs) == 1 {
		return msgs[0]
	}
	base := msgs[len(msgs)-1]
	texts := make([]string, 0, len(msgs))
	var files []slack.File
	for _, m := range msgs {
		if t := strings.TrimSpace(m.Text); t != "" {
			texts = append(texts, t)
		}
		files = append(files, m.Files...)
	}
	base.Text = strings.Join(texts, "\n\n")
	base.Files = files
	return base
}
