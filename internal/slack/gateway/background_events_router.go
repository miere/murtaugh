package gateway

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/slack/alertcard"
)

// defaultBackgroundIdleTimeout bounds a background stretch by inactivity, the
// same way defaultIdleTimeout bounds a chat turn — and it is deliberately the
// longer of the two.
//
// A chat turn's window is propped up while a tool runs: the backend ticks an
// EventStatus keep-alive at agent.DefaultToolHeartbeatInterval into the turn's
// event channel. A background stretch gets no such tick. The heartbeat writes to
// the turn's subscription (claudecode's `sub.events`), while OnBackground fires
// only when there is no subscription at all, so nothing keeps this window alive
// but real output: at the chat window's size, a silent tool run would read here
// as a stall. The tool ceiling is on the same subscription and does not bound a
// background stretch either, so this is the only bound there is — which is why it
// is not simply set to that ceiling: an open stretch holds a streaming Slack
// message open with it, and the value is a judgement about how long to leave one
// open, not a measurement.
//
// Overridden by defaults.session.background_idle_timeout, whose own default is
// this value.
const defaultBackgroundIdleTimeout = 15 * time.Minute

// backgroundStalledSpec is what the user sees when a background stretch is
// closed out for silence.
//
// It is handed to Finish, NOT to Fail, and that is the whole point of it. Fail
// paints the renderer's error card — level `error`, "Murtaugh hit an error while
// talking to the agent", "notify your admin user" (see failSpec) — and none of
// that is known to be true here: the router holds no session handle, so expiry
// closes a Slack message and nothing else. No work is cancelled, and a later
// event simply opens a fresh message in the same thread. The chat path makes the
// same distinction for the same reason, posting idleTimeoutAside rather than
// failing the turn.
//
// A warning and not a notice, on emptyReplySpec's terms: nothing broke, but a
// message that just stops needs explaining, and a message sealed in silence is
// indistinguishable from one that completed. Text is set so the card does not
// fall back to the level's generic guidance, which would be prescribing a remedy
// for a situation that needs none.
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
//
// # Liveness
//
// A background stretch is bounded by inactivity, because nothing else bounds it.
// The terminal events this router settles on are not guaranteed to arrive: a
// claude_code session that dies mid-stretch calls failActive, which returns
// without emitting anything when there is no active turn, so the stretch's
// EventError never reaches OnBackground. Without a window the renderer stays in
// `active` forever, holding a streaming Slack message open and being handed to
// the NEXT stretch for that conversation. The window is armed when the renderer
// is built, reset by every routed event, and disarmed when the stretch settles;
// on expiry the message is sealed with a notice below it and stopped — Finish,
// not Fail, because a stretch that went quiet is not an agent that failed
// (#192).
type backgroundEventsRouter struct {
	logger *slog.Logger

	// window is how long a stretch may go silent before it is closed out. Sized
	// against idle gaps rather than tool runtime — see defaultBackgroundIdleTimeout.
	window time.Duration

	// newRenderer builds a renderer for a thread — bound to ChatHandler.newChatRenderer
	// after the handler is constructed (Handle and the client are wired earlier).
	newRenderer func(string, string, StreamWriterOptions) chatRenderer

	mu      sync.Mutex
	targets map[string]bgTarget   // sessionID -> where to render
	active  map[string]*bgStretch // sessionID -> the in-progress background turn
}

// bgStretch is one background stretch: the renderer built for its first event,
// and the liveness window that closes the message if the stream goes quiet.
//
// mu is what makes the expiry goroutine legal. chatRenderer's contract is that it
// is driven from one goroutine at a time; here two can reach it — the client's
// per-session read loop, through Handle, and this stretch's watcher — so every
// call goes through render or end, which hold mu across the renderer call. The
// router's own mutex deliberately is NOT the one used: it guards the maps for
// every conversation, and holding it across a Slack write would put every other
// conversation behind that write.
type bgStretch struct {
	mu       sync.Mutex
	renderer chatRenderer
	// ended makes the terminal happen exactly once, whichever of a terminal event
	// and the expiry gets there first.
	ended  bool
	window time.Duration
	idle   *time.Timer
	// done releases the watcher goroutine when the stretch ends normally.
	done chan struct{}
}

// newBackgroundEventsRouter builds the router. A non-positive window falls back
// to defaultBackgroundIdleTimeout, matching ChatHandler.effectiveIdleTimeout.
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
// per-session read loop), so Handle itself is single-threaded per session; the
// stretch's own mutex is what keeps it apart from the expiry watcher, and the
// router mutex guards the maps across sessions.
func (b *backgroundEventsRouter) Handle(sessionID string, ev agent.Event) {
	ctx := context.Background()
	// The liveness window is reset FIRST, before the event's kind is looked at. A
	// reset that lived inside the switch would keep alive only the stretches that
	// happen to produce rendered output; here an event that renders nothing — a
	// payload-less task event, a kind a newer build knows and this one does not —
	// still counts as the stream moving. Nothing to reset before a stretch's first
	// event: the window is armed with the renderer, in stretchFor.
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

// stretchFor returns the session's in-progress background stretch, lazily
// creating one — renderer bound to the registered thread, liveness window armed —
// on the first event of a stretch. Returns nil when nothing is registered or the
// factory is not bound.
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

// take removes and returns the session's in-progress stretch, if any.
func (b *backgroundEventsRouter) take(sessionID string) *bgStretch {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.active[sessionID]
	delete(b.active, sessionID)
	return s
}

// keepAlive restarts the window of whatever stretch the session has open. A
// session with no stretch in progress has no window, and events for an
// unregistered session never open one.
func (b *backgroundEventsRouter) keepAlive(sessionID string) {
	b.mu.Lock()
	s := b.active[sessionID]
	b.mu.Unlock()
	if s != nil {
		s.keepAlive()
	}
}

// watchLiveness closes a stretch out if its window elapses. One goroutine per
// stretch, started with the renderer and released by end. It exists because
// Handle is a callback handed one event at a time: there is no loop here to
// select the timer in, the way ChatHandler's turn loop selects its own.
func (b *backgroundEventsRouter) watchLiveness(sessionID string, s *bgStretch) {
	select {
	case <-s.done:
	case <-s.idle.C:
		b.expire(sessionID, s)
	}
}

// expire ends a stretch that went silent for a whole window: the message is
// sealed with the stall notice below it and then stopped — the structure an
// EventError gets, because a message sealed in silence would look exactly like
// one that completed, but through Finish rather than Fail, because going quiet
// is not the agent failing (see backgroundStalledSpec) — and the stretch is
// retired so the next background event opens a fresh one.
//
// The session's TARGET is deliberately left registered. It is the thread, it is
// still valid, and dropping it would silently disable background delivery for
// that conversation from here on.
func (b *backgroundEventsRouter) expire(sessionID string, s *bgStretch) {
	b.mu.Lock()
	// Only if it is still the current stretch: a terminal event may have taken it
	// and a later one replaced it while this goroutine was waking up.
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
		// A terminal event won the race and has already settled the message.
		return
	}
	b.logger.Warn("background stretch went silent; closed out its message", "session", sessionID, "window", s.window)
}

// keepAlive restarts the stretch's liveness window.
func (s *bgStretch) keepAlive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	resetIdleTimer(s.idle, s.window)
}

// render drives the stretch's renderer, unless the stretch has already ended.
// Reports whether fn ran; an event that arrives in the instant between the
// window elapsing and the stretch being retired is dropped rather than written
// to a message that has just been sealed.
func (s *bgStretch) render(fn func(chatRenderer)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return false
	}
	fn(s.renderer)
	return true
}

// end settles the stretch exactly once, disarming the window and releasing the
// watcher. Reports whether fn ran: false means the stretch was already settled by
// the other of the two contenders (a terminal event, or the expiry).
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
