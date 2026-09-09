package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// defaultBusyTimeout is how long a session may hold a turn open before the
// manager stops believing it is working and reaps it. It is deliberately long:
// an agent grinding overnight on a delegated task is expected behaviour, not a
// fault, so this bounds a genuinely wedged session rather than a slow one.
const defaultBusyTimeout = 18 * time.Hour

type SessionManager struct {
	client      Client
	idleTimeout time.Duration
	busyTimeout time.Duration
	maxSessions int
	now         func() time.Time
	logger      *slog.Logger
	// onEvict, when set, is notified for every dropped session. It is called
	// while the manager's lock is held, so it MUST NOT block — the gateway wires
	// it to a journal recorder whose Record is a non-blocking enqueue.
	onEvict func(Eviction)

	// kind and approval describe the backend this manager drives, for logging
	// only. They keep the start/session lines backend-accurate (this manager is
	// generic — it drives native, acp, and claude_code alike). Empty until set
	// via WithDescriptor; the alias travels on the logger as the `agent` attr.
	kind     string
	approval string

	// cancelOverride, when non-nil, forces the interruptible verdict and
	// skips the startup probe. It is populated from the agent's
	// `interruptible:` config flag.
	cancelOverride *bool

	mu          sync.Mutex
	initialized bool
	// interruptible caches whether the agent implements session/cancel.
	// interruptibleKnown is false until Warm has resolved it (via the
	// override or the probe); callers treat unknown as interruptible so a
	// missing/failed probe never silently changes behaviour.
	interruptible      bool
	interruptibleKnown bool
	sessions           map[ConversationKey]managedSession
}

// cancelCapabilityProber is the optional capability surface a Client can
// implement so the manager can detect, at warmup, whether the agent supports
// interruption. *ProcessClient satisfies it.
type cancelCapabilityProber interface {
	SupportsCancel(ctx context.Context) bool
}

// sessionCloser is the optional surface a Client implements when each session owns
// a dedicated resource (e.g. a per-conversation process) that must be released
// when the manager evicts or discards the conversation. A client that multiplexes
// many sessions over one process/loop does not implement it, so the manager's
// calls are simply no-ops there.
type sessionCloser interface {
	CloseSession(sessionID string)
}

// closeClientSession releases the client-side resources for a session when the
// client owns per-session state. Safe to call for any backend.
func (m *SessionManager) closeClientSession(sessionID string) {
	if closer, ok := m.client.(sessionCloser); ok {
		closer.CloseSession(sessionID)
	}
}

// EvictionReason names why the manager dropped a cached session, so an operator
// reading the log or the journal can tell an expected sweep from a capacity kill
// without reading this file.
type EvictionReason string

const (
	// EvictionIdle is the ordinary sweep: no turn in flight and nothing said for
	// idle_timeout. Expected and unremarkable — the next message in that thread
	// opens a fresh session — so it is never surfaced to the user.
	EvictionIdle EvictionReason = "idle"
	// EvictionBusyTimeout is the runaway guard: a session that has held a turn
	// open for longer than busy_timeout is presumed wedged rather than working.
	EvictionBusyTimeout EvictionReason = "busy_timeout"
	// EvictionCapacity is an idle session dropped to free a slot at max_sessions.
	// A working session is never taken this way; when every slot is busy the new
	// conversation is refused instead — see CapacityError.
	EvictionCapacity EvictionReason = "capacity"
)

// Eviction describes one dropped session for the observer.
type Eviction struct {
	Key       ConversationKey
	SessionID string
	Reason    EvictionReason
	// Age is how long the session had been in the state that condemned it: time
	// since last use for an idle or capacity eviction, time since the turn began
	// for a busy-timeout one.
	Age time.Duration
	// Live is how many sessions the manager still holds after the drop.
	Live int
}

// CapacityError is returned when every one of the manager's max_sessions slots
// is running a turn, so there is nothing idle to evict and no room for the new
// conversation. It is a refusal, not a fault: the honest answer is that the
// agent is full, and the alternative — taking the slot from a session that is
// mid-work — is the bug this type exists to prevent.
type CapacityError struct {
	// Limit is the configured max_sessions the manager is up against.
	Limit int
}

func (e *CapacityError) Error() string {
	return fmt.Sprintf("agent is at capacity: all %d session slots are running a turn", e.Limit)
}

// AtCapacity reports whether err is (or wraps) a CapacityError, so a caller can
// render the refusal as a warning rather than as something that broke.
func AtCapacity(err error) (*CapacityError, bool) {
	var capacity *CapacityError
	if errors.As(err, &capacity) {
		return capacity, true
	}
	return nil, false
}

type managedSession struct {
	session Session
	// lastUsed starts the idle clock. It is stamped when a turn ENDS as well as
	// when one begins, so a session that worked for three hours gets a full idle
	// window afterwards instead of being sweepable the moment it falls quiet.
	lastUsed time.Time
	// active counts turns in flight on this session. Any value above zero means
	// the conversation is working, and working is not idle — the distinction the
	// idle sweep used to lack, which is how it came to kill live turns.
	active int
	// busySince is when the session went from quiet to working (active 0→1). It
	// is the clock busy_timeout runs against; zero while the session is idle.
	busySince time.Time
}

func NewSessionManager(client Client, idleTimeout time.Duration, maxSessions int) *SessionManager {
	if idleTimeout <= 0 {
		idleTimeout = 30 * time.Minute
	}
	if maxSessions <= 0 {
		maxSessions = 100
	}
	return &SessionManager{client: client, idleTimeout: idleTimeout, busyTimeout: defaultBusyTimeout, maxSessions: maxSessions, now: time.Now, logger: slog.Default(), sessions: make(map[ConversationKey]managedSession)}
}

// WithBusyTimeout overrides how long a session may hold a turn open before it is
// treated as wedged. Zero or negative leaves the default in place.
func (m *SessionManager) WithBusyTimeout(d time.Duration) *SessionManager {
	if d > 0 {
		m.busyTimeout = d
	}
	return m
}

// WithEvictionObserver registers a callback fired for every dropped session. The
// callback runs under the manager's lock and must not block.
func (m *SessionManager) WithEvictionObserver(fn func(Eviction)) *SessionManager {
	m.onEvict = fn
	return m
}

func (m *SessionManager) WithLogger(logger *slog.Logger) *SessionManager {
	if logger != nil {
		m.logger = logger
	}
	return m
}

// WithCancelOverride forces the interruptible verdict from configuration,
// bypassing the startup probe. nil leaves auto-detection in place.
func (m *SessionManager) WithCancelOverride(override *bool) *SessionManager {
	m.cancelOverride = override
	return m
}

// WithDescriptor records the backend kind and approval posture so the manager's
// log lines name the actual backend instead of a hard-coded "ACP". Purely for
// diagnostics; behaviour is unaffected.
func (m *SessionManager) WithDescriptor(kind, approval string) *SessionManager {
	m.kind = kind
	m.approval = approval
	return m
}

// Interruptible reports whether the agent can have an in-flight prompt
// cancelled via session/cancel. Until Warm has resolved the capability it
// returns true so behaviour is unchanged when detection has not (yet) run.
func (m *SessionManager) Interruptible() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.interruptibleKnown {
		return true
	}
	return m.interruptible
}

// resolveInterruptible determines and caches whether the agent supports
// cancellation: the config override wins; otherwise it probes the client when
// the client exposes the capability surface. Defaults to interruptible when no
// signal is available. Logs the verdict once so operators see it at deployment.
func (m *SessionManager) resolveInterruptible(ctx context.Context) {
	interruptible := true
	source := "default"
	if m.cancelOverride != nil {
		interruptible = *m.cancelOverride
		source = "config"
	} else if prober, ok := m.client.(cancelCapabilityProber); ok {
		interruptible = prober.SupportsCancel(ctx)
		source = "probe"
	}
	m.mu.Lock()
	m.interruptible = interruptible
	m.interruptibleKnown = true
	m.mu.Unlock()
	if interruptible {
		m.logger.Info("agent is interruptible", "type", m.kind, "source", source)
		return
	}
	m.logger.Warn("agent does not support cancellation; new messages will not interrupt an in-flight response", "type", m.kind, "source", source)
}

func (m *SessionManager) Warm(ctx context.Context) error {
	m.mu.Lock()
	if m.initialized {
		m.mu.Unlock()
		return nil
	}
	startedAt := m.now()
	if err := m.client.Initialize(ctx); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("initialize agent client: %w", err)
	}
	m.initialized = true
	m.mu.Unlock()
	// The one line that names the backend at startup, so the maintainer can tell
	// native/acp/claude_code apart at a glance (the alias rides as `agent`).
	m.logger.Info("agent started", "type", m.kind, "approval", m.approval, "duration", m.now().Sub(startedAt))
	// Resolve interruptibility after releasing the lock: the probe performs a
	// round-trip to the agent and resolveInterruptible takes the lock itself.
	m.resolveInterruptible(ctx)
	return nil
}

func (m *SessionManager) Prompt(ctx context.Context, key ConversationKey, metadata SessionMetadata, request PromptRequest) (<-chan Event, error) {
	session, err := m.session(ctx, key, metadata)
	if err != nil {
		return nil, err
	}
	// Tell the agent which Slack conversation it is in so it can target the
	// `restart` tool's approval card here instead of the admin DM. Only fill
	// what the caller has not already set, so explicit callers win.
	if request.Channel == "" {
		request.Channel = metadata.ChannelID
	}
	if request.Thread == "" {
		request.Thread = metadata.ThreadTS
	}
	if request.User == "" {
		request.User = metadata.UserID
	}
	events, err := m.client.Prompt(ctx, session.ID, request)
	if err != nil {
		// The turn was marked busy when the session was acquired; it never
		// started, so release it here rather than leaving the slot pinned.
		m.endTurn(key, session.ID)
		return nil, err
	}
	return m.trackTurn(key, session.ID, events), nil
}

// trackTurn forwards the client's event channel and releases the turn when it
// closes, so a session counts as busy for exactly as long as the agent is
// talking.
//
// It leans on the invariant the agent layer already depends on: a caller drains
// the channel until close (abandoning it stalls the shared read loop for every
// other conversation — see the drain in ChatHandler's idle path). A caller that
// broke it would pin this session as busy indefinitely, which is what
// busy_timeout is there to unpin.
func (m *SessionManager) trackTurn(key ConversationKey, sessionID string, in <-chan Event) <-chan Event {
	out := make(chan Event)
	go func() {
		// Defer order is deliberate (LIFO): endTurn runs BEFORE out closes, so a
		// caller that has drained to close is guaranteed the session is already
		// back on the idle clock — a follow-up prompt cannot observe a turn that
		// has visibly finished still counted as in flight.
		defer close(out)
		defer m.endTurn(key, sessionID)
		for event := range in {
			out <- event
		}
	}()
	return out
}

// beginTurnLocked marks the conversation as working. The caller holds the lock,
// and does so across session acquisition, so there is no window in which a
// freshly acquired session looks idle to a concurrent sweep.
func (m *SessionManager) beginTurnLocked(key ConversationKey) {
	session, ok := m.sessions[key]
	if !ok {
		return
	}
	if session.active == 0 {
		session.busySince = m.now()
	}
	session.active++
	m.sessions[key] = session
}

// endTurn releases one in-flight turn. sessionID guards the decrement: a turn
// whose session has since been discarded and replaced for the same conversation
// must not decrement — or unmark — its successor.
func (m *SessionManager) endTurn(key ConversationKey, sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[key]
	if !ok || session.session.ID != sessionID || session.active == 0 {
		return
	}
	session.active--
	if session.active == 0 {
		session.lastUsed = m.now()
		session.busySince = time.Time{}
	}
	m.sessions[key] = session
}

// Lookup returns the cached session ID for the conversation without
// creating a new session. The second return value is false when the
// conversation has no live session — callers must treat that case as
// "nothing to cancel" and skip the ACP-level cancel call.
func (m *SessionManager) Lookup(key ConversationKey) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[key]
	if !ok {
		return "", false
	}
	return session.session.ID, true
}

// Cancel asks the underlying ACP client to abort the given session's
// in-flight prompt. It is a thin pass-through so callers (e.g. the Slack
// frontend's interrupt registry) do not need to hold a reference to the
// raw Client. The cancel call is best-effort: the agent is free to
// finish flushing trailing chunks before honouring the request.
func (m *SessionManager) Cancel(ctx context.Context, sessionID string) error {
	return m.client.Cancel(ctx, sessionID)
}

// Discard forgets the cached session for a conversation so the next prompt opens
// a fresh session/new instead of reusing it. Used when a turn is abandoned on the
// idle watchdog: the agent may have left an in-flight tool call wedged in that
// session (and agents that lack session/cancel cannot be told to drop it), so
// reusing it risks inheriting the stall. The underlying agent process — shared
// across every conversation — is left running; only this conversation's binding
// is reset. No-op when the conversation has no live session.
func (m *SessionManager) Discard(key ConversationKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if session, ok := m.sessions[key]; ok {
		m.closeClientSession(session.session.ID)
		delete(m.sessions, key)
	}
}

// session resolves the conversation's session — reusing the cached one or
// opening a new one — and returns it already marked as working. Every return
// path leaves exactly one in-flight turn recorded against the session, which the
// caller must release through endTurn (Prompt does, via trackTurn).
func (m *SessionManager) session(ctx context.Context, key ConversationKey, metadata SessionMetadata) (Session, error) {
	m.mu.Lock()
	if session, ok := m.sessions[key]; ok {
		session.lastUsed = m.now()
		m.sessions[key] = session
		m.beginTurnLocked(key)
		m.mu.Unlock()
		m.logger.Info("reusing agent session", "type", m.kind, "team", key.TeamID, "channel", key.ChannelID, "thread", key.ThreadTS, "dm", key.DM)
		return session.session, nil
	}
	m.mu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if session, ok := m.sessions[key]; ok {
		session.lastUsed = m.now()
		m.sessions[key] = session
		m.beginTurnLocked(key)
		m.logger.Info("reusing agent session", "type", m.kind, "team", key.TeamID, "channel", key.ChannelID, "thread", key.ThreadTS, "dm", key.DM)
		return session.session, nil
	}
	if !m.initialized {
		if err := m.client.Initialize(ctx); err != nil {
			return Session{}, fmt.Errorf("initialize agent client: %w", err)
		}
		m.initialized = true
	}
	if err := m.evictLocked(); err != nil {
		return Session{}, err
	}
	startedAt := m.now()
	session, err := m.client.NewSession(ctx, metadata)
	if err != nil {
		return Session{}, fmt.Errorf("create agent session: %w", err)
	}
	m.sessions[key] = managedSession{session: session, lastUsed: m.now()}
	m.beginTurnLocked(key)
	m.logger.Info("created agent session", "type", m.kind, "team", key.TeamID, "channel", key.ChannelID, "thread", key.ThreadTS, "dm", key.DM, "duration", m.now().Sub(startedAt))
	return session, nil
}

// evictLocked makes room for a new conversation, and is the one place a cached
// session is dropped without being asked for.
//
// Its governing rule is that a session with a turn in flight is NOT idle. The
// sweep reads `active` first and only then reaches for a clock, so the only
// thing that can take a working session is busy_timeout — the runaway guard —
// and never the ordinary idle sweep or the capacity squeeze. When every slot is
// busy it returns *CapacityError rather than evicting someone's live turn.
//
// It runs lazily, on new-session requests, so an idle or runaway session is
// reaped the next time somebody starts a conversation with this agent rather
// than on a timer.
func (m *SessionManager) evictLocked() error {
	now := m.now()
	for key, session := range m.sessions {
		if session.active > 0 {
			if age := now.Sub(session.busySince); age > m.busyTimeout {
				m.dropLocked(key, session, EvictionBusyTimeout, age)
			}
			continue
		}
		if age := now.Sub(session.lastUsed); age > m.idleTimeout {
			m.dropLocked(key, session, EvictionIdle, age)
		}
	}
	for len(m.sessions) >= m.maxSessions {
		key, session, ok := m.oldestIdleLocked()
		if !ok {
			return &CapacityError{Limit: m.maxSessions}
		}
		m.dropLocked(key, session, EvictionCapacity, now.Sub(session.lastUsed))
	}
	return nil
}

// oldestIdleLocked returns the least-recently-used session that is NOT working,
// which is the only kind the capacity squeeze is allowed to take. The bool is
// false when every cached session has a turn in flight.
func (m *SessionManager) oldestIdleLocked() (ConversationKey, managedSession, bool) {
	var oldestKey ConversationKey
	var oldest managedSession
	found := false
	for key, session := range m.sessions {
		if session.active > 0 {
			continue
		}
		if !found || session.lastUsed.Before(oldest.lastUsed) {
			oldestKey, oldest, found = key, session, true
		}
	}
	return oldestKey, oldest, found
}

// dropLocked releases one session and reports it. Every eviction says which of
// the three reasons condemned it, in the log and (when wired) in the journal —
// without that line an operator can only infer a sweep from its side effects.
func (m *SessionManager) dropLocked(key ConversationKey, session managedSession, reason EvictionReason, age time.Duration) {
	m.closeClientSession(session.session.ID)
	delete(m.sessions, key)
	args := []any{
		"type", m.kind, "reason", string(reason), "age", age,
		"team", key.TeamID, "channel", key.ChannelID, "thread", key.ThreadTS, "dm", key.DM,
		"session_id", session.session.ID, "live", len(m.sessions),
	}
	if reason == EvictionBusyTimeout {
		// The only eviction that interrupts work, so the only one worth a warning.
		m.logger.Warn("evicted agent session mid-turn on busy timeout", args...)
	} else {
		m.logger.Info("evicted agent session", args...)
	}
	if m.onEvict != nil {
		m.onEvict(Eviction{Key: key, SessionID: session.session.ID, Reason: reason, Age: age, Live: len(m.sessions)})
	}
}

func (m *SessionManager) Close() error {
	return m.client.Close()
}
