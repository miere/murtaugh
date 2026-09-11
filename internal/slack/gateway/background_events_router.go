package gateway

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/slack/alertcard"
)

const defaultBackgroundIdleTimeout = 15 * time.Minute

func backgroundStalledSpec() alertcard.Spec {
	return alertcard.Spec{
		Level:    alertcard.LevelWarn,
		Title:    "Closed out a background update",
		Subtitle: "This background update went quiet and never finished, so I've closed it out.",
		Text:     "_Nothing was cancelled — nudge me if you want it picked back up._",
	}
}

type bgTarget struct {
	channelID  string
	threadTS   string
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

	window time.Duration

	// newRenderer builds a renderer for a thread — bound to ChatHandler.newChatRenderer
	// after the handler is constructed (Handle and the client are wired earlier).
	newRenderer func(string, string, StreamWriterOptions) chatRenderer

	mu      sync.Mutex
	targets map[string]bgTarget
	active  map[string]*bgStretch
}

type bgStretch struct {
	mu       sync.Mutex
	renderer chatRenderer
	ended    bool
	window   time.Duration
	idle     *time.Timer
	done     chan struct{}
}

func newBackgroundEventsRouter(logger *slog.Logger, window time.Duration) *backgroundEventsRouter {
	if logger == nil {
		logger = slog.Default()
	}
	if window <= 0 {
		window = defaultBackgroundIdleTimeout
	}
	return &backgroundEventsRouter{logger: logger, window: window, targets: map[string]bgTarget{}, active: map[string]*bgStretch{}}
}

// bind supplies the renderer factory once the ChatHandler exists. Safe to call
// before any background event fires (which only happens once a real turn runs).
func (b *backgroundEventsRouter) bind(newRenderer func(string, string, StreamWriterOptions) chatRenderer) {
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
// per-session read loop).
func (b *backgroundEventsRouter) Handle(sessionID string, ev agent.Event) {
	ctx := context.Background()
	b.keepAlive(sessionID)

	switch ev.Type {
	case agent.EventText:
		if s := b.stretchFor(sessionID); s != nil {
			s.render(func(r chatRenderer) { _ = r.Text(ctx, ev.Text) })
		}
	case agent.EventTask:
		if ev.Task == nil {
			return
		}
		if s := b.stretchFor(sessionID); s != nil {
			s.render(func(r chatRenderer) { _ = r.Task(ctx, ev.Task) })
		}
	case agent.EventAttachment:
		if ev.Attachment == nil {
			return
		}
		if s := b.stretchFor(sessionID); s != nil {
			s.render(func(r chatRenderer) { _ = r.Attachment(ctx, ev.Attachment) })
		}
	case agent.EventComplete:
		if s := b.take(sessionID); s != nil {
			s.end(func(r chatRenderer) {
				_ = r.Finish(ctx, nil)
				r.EnsureStopped(ctx)
			})
		}
	case agent.EventError:
		if s := b.take(sessionID); s != nil {
			s.end(func(r chatRenderer) {
				_ = r.Fail(ctx, ev.Error)
				r.EnsureStopped(ctx)
			})
		}
	}
}

func (b *backgroundEventsRouter) stretchFor(sessionID string) *bgStretch {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.active[sessionID]; ok {
		return s
	}
	t, ok := b.targets[sessionID]
	if !ok || b.newRenderer == nil {
		b.logger.Debug("background event with no rendering target; dropping", "session", sessionID)
		return nil
	}
	s := &bgStretch{
		renderer: b.newRenderer(t.channelID, t.threadTS, t.streamOpts),
		window:   b.window,
		idle:     time.NewTimer(b.window),
		done:     make(chan struct{}),
	}
	b.active[sessionID] = s
	go b.watchLiveness(sessionID, s)
	b.logger.Info("rendering background completion into thread", "session", sessionID, "channel", t.channelID, "thread", t.threadTS)
	return s
}

func (b *backgroundEventsRouter) take(sessionID string) *bgStretch {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.active[sessionID]
	delete(b.active, sessionID)
	return s
}

func (b *backgroundEventsRouter) keepAlive(sessionID string) {
	b.mu.Lock()
	s := b.active[sessionID]
	b.mu.Unlock()
	if s != nil {
		s.keepAlive()
	}
}

func (b *backgroundEventsRouter) watchLiveness(sessionID string, s *bgStretch) {
	select {
	case <-s.done:
	case <-s.idle.C:
		b.expire(sessionID, s)
	}
}

func (b *backgroundEventsRouter) expire(sessionID string, s *bgStretch) {
	b.mu.Lock()
	if b.active[sessionID] == s {
		delete(b.active, sessionID)
	}
	b.mu.Unlock()
	ctx := context.Background()
	stalled := backgroundStalledSpec()
	settled := s.end(func(r chatRenderer) {
		_ = r.Finish(ctx, &stalled)
		r.EnsureStopped(ctx)
	})
	if !settled {
		return
	}
	b.logger.Warn("background stretch went silent; closed out its message", "session", sessionID, "window", s.window)
}

func (s *bgStretch) keepAlive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	resetIdleTimer(s.idle, s.window)
}

func (s *bgStretch) render(fn func(chatRenderer)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return false
	}
	fn(s.renderer)
	return true
}

func (s *bgStretch) end(fn func(chatRenderer)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return false
	}
	s.ended = true
	s.idle.Stop()
	close(s.done)
	fn(s.renderer)
	return true
}
