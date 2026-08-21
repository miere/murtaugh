package gateway

import (
	"context"
	"log/slog"
	"sync"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/config"
)

// bgTarget is where — and how — a conversation's background completions render.
// The thread never changes for a conversation, but the progress mode / stream
// options are captured per turn so a background reply matches the foreground one.
type bgTarget struct {
	channelID  string
	threadTS   string
	mode       config.ProgressDisplay
	streamOpts StreamWriterOptions
}

// backgroundEventsRouter routes a claude_code background turn — a subagent completing
// after its turn ended, then the model auto-continuing — into the Slack thread it
// belongs to, driving the SAME chatRenderer a foreground turn uses. So a
// background reply looks identical: streamed text, task cards, attachments,
// finalisation. This is the delivery that closes the silent-treatment loop
// (spec 019 §5): work that finishes after the turn returns still reaches the human.
//
// One router is shared across every claude_code agent; conversations are keyed by
// their deterministic session id (agent.DeriveSessionID), which is exactly the id
// the client's OnBackground fires with, so registration and delivery line up.
type backgroundEventsRouter struct {
	logger *slog.Logger

	// newRenderer builds a renderer for a thread — bound to ChatHandler.newChatRenderer
	// after the handler is constructed (Handle and the client are wired earlier).
	newRenderer func(config.ProgressDisplay, string, string, StreamWriterOptions) chatRenderer

	mu      sync.Mutex
	targets map[string]bgTarget     // sessionID -> where to render
	active  map[string]chatRenderer // sessionID -> the in-progress background turn
}

func newBackgroundEventsRouter(logger *slog.Logger) *backgroundEventsRouter {
	if logger == nil {
		logger = slog.Default()
	}
	return &backgroundEventsRouter{logger: logger, targets: map[string]bgTarget{}, active: map[string]chatRenderer{}}
}

// bind supplies the renderer factory once the ChatHandler exists. Safe to call
// before any background event fires (which only happens once a real turn runs).
func (b *backgroundEventsRouter) bind(newRenderer func(config.ProgressDisplay, string, string, StreamWriterOptions) chatRenderer) {
	b.mu.Lock()
	b.newRenderer = newRenderer
	b.mu.Unlock()
}

// Register records where a conversation's background completions should render.
// Called by Handle each turn so the target (thread + rendering options) stays
// current. Without a registered target, a background event is dropped — there is
// nowhere to post it.
func (b *backgroundEventsRouter) Register(sessionID string, t bgTarget) {
	b.mu.Lock()
	b.targets[sessionID] = t
	b.mu.Unlock()
}

// Handle is the claude_code OnBackground target: events a session emits with no
// active foreground turn. Events for one session arrive serialised (the client's
// per-session read loop), so a session's renderer is only ever driven by one
// goroutine; the mutex guards the maps across sessions.
func (b *backgroundEventsRouter) Handle(sessionID string, ev agent.Event) {
	ctx := context.Background()
	switch ev.Type {
	case agent.EventText:
		if r := b.rendererFor(sessionID); r != nil {
			_ = r.Text(ctx, ev.Text)
		}
	case agent.EventTask:
		if ev.Task == nil {
			return
		}
		if r := b.rendererFor(sessionID); r != nil {
			_ = r.Task(ctx, ev.Task)
		}
	case agent.EventAttachment:
		if ev.Attachment == nil {
			return
		}
		if r := b.rendererFor(sessionID); r != nil {
			_ = r.Attachment(ctx, ev.Attachment)
		}
	case agent.EventComplete:
		if r := b.take(sessionID); r != nil {
			_ = r.Finish(ctx, nil)
			r.EnsureStopped(ctx)
		}
	case agent.EventError:
		if r := b.take(sessionID); r != nil {
			_ = r.Fail(ctx, ev.Error)
			r.EnsureStopped(ctx)
		}
	}
}

// rendererFor returns the session's in-progress background renderer, lazily
// creating one (bound to its registered thread) on the first event of a turn.
// Returns nil when nothing is registered or the factory is not bound.
func (b *backgroundEventsRouter) rendererFor(sessionID string) chatRenderer {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.active[sessionID]; ok {
		return r
	}
	t, ok := b.targets[sessionID]
	if !ok || b.newRenderer == nil {
		b.logger.Debug("background event with no rendering target; dropping", "session", sessionID)
		return nil
	}
	r := b.newRenderer(t.mode, t.channelID, t.threadTS, t.streamOpts)
	b.active[sessionID] = r
	b.logger.Info("rendering background completion into thread", "session", sessionID, "channel", t.channelID, "thread", t.threadTS)
	return r
}

// take removes and returns the session's in-progress renderer, if any.
func (b *backgroundEventsRouter) take(sessionID string) chatRenderer {
	b.mu.Lock()
	defer b.mu.Unlock()
	r := b.active[sessionID]
	delete(b.active, sessionID)
	return r
}
