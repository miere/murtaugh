package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type SessionManager struct {
	client      Client
	idleTimeout time.Duration
	maxSessions int
	now         func() time.Time
	logger      *slog.Logger

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

type managedSession struct {
	session  Session
	lastUsed time.Time
}

func NewSessionManager(client Client, idleTimeout time.Duration, maxSessions int) *SessionManager {
	if idleTimeout <= 0 {
		idleTimeout = 30 * time.Minute
	}
	if maxSessions <= 0 {
		maxSessions = 100
	}
	return &SessionManager{client: client, idleTimeout: idleTimeout, maxSessions: maxSessions, now: time.Now, logger: slog.Default(), sessions: make(map[ConversationKey]managedSession)}
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
	return m.client.Prompt(ctx, session.ID, request)
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

func (m *SessionManager) session(ctx context.Context, key ConversationKey, metadata SessionMetadata) (Session, error) {
	m.mu.Lock()
	if session, ok := m.sessions[key]; ok {
		session.lastUsed = m.now()
		m.sessions[key] = session
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
		m.logger.Info("reusing agent session", "type", m.kind, "team", key.TeamID, "channel", key.ChannelID, "thread", key.ThreadTS, "dm", key.DM)
		return session.session, nil
	}
	if !m.initialized {
		if err := m.client.Initialize(ctx); err != nil {
			return Session{}, fmt.Errorf("initialize agent client: %w", err)
		}
		m.initialized = true
	}
	m.evictLocked()
	startedAt := m.now()
	session, err := m.client.NewSession(ctx, metadata)
	if err != nil {
		return Session{}, fmt.Errorf("create agent session: %w", err)
	}
	m.sessions[key] = managedSession{session: session, lastUsed: m.now()}
	m.logger.Info("created agent session", "type", m.kind, "team", key.TeamID, "channel", key.ChannelID, "thread", key.ThreadTS, "dm", key.DM, "duration", m.now().Sub(startedAt))
	return session, nil
}

func (m *SessionManager) evictLocked() {
	now := m.now()
	for key, session := range m.sessions {
		if now.Sub(session.lastUsed) > m.idleTimeout {
			m.closeClientSession(session.session.ID)
			delete(m.sessions, key)
		}
	}
	for len(m.sessions) >= m.maxSessions {
		var oldestKey ConversationKey
		var oldest time.Time
		first := true
		for key, session := range m.sessions {
			if first || session.lastUsed.Before(oldest) {
				oldestKey = key
				oldest = session.lastUsed
				first = false
			}
		}
		m.closeClientSession(m.sessions[oldestKey].session.ID)
		delete(m.sessions, oldestKey)
	}
}

func (m *SessionManager) Close() error {
	return m.client.Close()
}
