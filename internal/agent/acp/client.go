package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

type ProcessOptions struct {
	Command string
	Args    []string
	WorkDir string
	// Env are extra KEY=VALUE entries layered on top of the inherited environment
	// when the agent process is started. Empty leaves the process with Murtaugh's
	// own environment unchanged.
	Env    []string
	Logger *slog.Logger

	// PermissionPolicy governs how agent-initiated session/request_permission
	// requests are answered: "ask" (raise an agent.EventPermission on the turn's
	// event stream for the consumer to resolve with a human), "auto-allow", or
	// "auto-deny". Empty is treated as "ask". "ask" with no live turn consuming
	// events (headless/CLI) denies — fail-safe and fast, never a hang.
	PermissionPolicy string
	// Aggregator, when set, registers each session with Murtaugh's per-agent MCP
	// aggregator and supplies the stdio bridge server advertised in session/new, so
	// the agent can reach Murtaugh's own tools. nil leaves mcpServers empty. One
	// aggregator is shared across all of an agent's sessions; the manager closes it.
	Aggregator agent.Aggregator
	// Persona is Murtaugh's shared persona (SOUL.md). ACP exposes no system role,
	// so when set it is injected as a leading <persona> block on every prompt.
	Persona string
	// ToolHeartbeatInterval is how often a still-running tool emits a keep-alive
	// status event so the gateway's idle watchdog does not treat a long,
	// output-silent tool call as a stall. Zero takes defaultACPToolHeartbeatInterval.
	ToolHeartbeatInterval time.Duration
	// ToolCeiling bounds how long a single tool call may hold a turn before it is
	// failed with ErrToolCeiling. Zero takes defaultACPToolCeiling; a negative
	// value disables the ceiling.
	ToolCeiling time.Duration
}

// Client is the ACP backend: a manager that runs ONE agent process per
// conversation (an acpSession each), so concurrent Slack threads are isolated —
// separate processes, separate ACP sessions, no shared-process taint. It
// implements agent.Client; the gateway shares one Client per agent and this
// manager fans conversations out to their own processes.
type Client struct {
	opts ProcessOptions
	log  *slog.Logger

	mu       sync.Mutex
	closed   bool
	sessions map[string]*acpSession

	// caps and interruptible are resolved once at Initialize (a throwaway probe
	// process) and cached — they are properties of the agent binary, identical
	// across every session's process. probed guards the one-time resolution.
	caps          AgentCapabilities
	interruptible bool
	probed        bool
}

// NewProcessClient builds the ACP manager. (Name kept for the agentbuild seam.)
func NewProcessClient(opts ProcessOptions) *Client {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{opts: opts, log: logger, sessions: make(map[string]*acpSession)}
}

// Initialize validates the agent and resolves its capabilities once, via a
// short-lived probe process (which also surfaces a bad command at startup rather
// than on the first conversation). Per-conversation processes start lazily in
// NewSession.
func (c *Client) Initialize(ctx context.Context) error {
	startedAt := time.Now()
	probe := newACPSession(c.opts)
	caps, err := probe.bringUp(ctx)
	if err != nil {
		probe.close()
		return err
	}
	interruptible := probe.supportsCancel(ctx)
	probe.close()

	c.mu.Lock()
	c.caps = caps
	c.interruptible = interruptible
	c.probed = true
	c.mu.Unlock()
	c.log.Info("initialized ACP client",
		"duration", time.Since(startedAt),
		"protocol_version", caps.ProtocolVersion,
		"mcp_http", caps.MCP.HTTP,
		"mcp_sse", caps.MCP.SSE,
		"load_session", caps.LoadSession,
		"interruptible", interruptible,
	)
	return nil
}

// NewSession starts a fresh process for this conversation, performs the ACP
// handshake and session/new, and registers it under the deterministic
// conversation id (agent.DeriveSessionID). It is NOT keyed by the agent-returned
// session id: that is only unique within one agent process, and every
// conversation now has its own process, so two conversations could be handed the
// same agent id and collide. The agent's id is used only for this session's own
// ACP calls (held on acpSession.sessionID).
func (c *Client) NewSession(ctx context.Context, meta agent.SessionMetadata) (agent.Session, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return agent.Session{}, errors.New("ACP client is closed")
	}
	c.mu.Unlock()

	s := newACPSession(c.opts)
	if _, err := s.bringUp(ctx); err != nil {
		s.close()
		return agent.Session{}, err
	}
	if _, err := s.openSession(ctx, meta); err != nil {
		s.close()
		return agent.Session{}, fmt.Errorf("create ACP session: %w", err)
	}
	// Route by the deterministic conversation id — unique per conversation, so it
	// never collides on the agent's (per-process) session id.
	key := agent.DeriveSessionID(meta)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		s.close()
		return agent.Session{}, errors.New("ACP client is closed")
	}
	c.sessions[key] = s
	c.mu.Unlock()
	return agent.Session{ID: key}, nil
}

// Prompt routes a turn to the identified conversation's process.
func (c *Client) Prompt(ctx context.Context, sessionID string, request agent.PromptRequest) (<-chan agent.Event, error) {
	c.mu.Lock()
	s, ok := c.sessions[sessionID]
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("ACP: no session %q", sessionID)
	}
	return s.prompt(ctx, request)
}

// Cancel interrupts the identified conversation's in-flight turn.
func (c *Client) Cancel(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	s, ok := c.sessions[sessionID]
	c.mu.Unlock()
	if !ok {
		return nil
	}
	return s.cancelTurn(ctx)
}

// CloseSession tears down one conversation's process (e.g. on idle eviction).
func (c *Client) CloseSession(sessionID string) {
	c.mu.Lock()
	s := c.sessions[sessionID]
	delete(c.sessions, sessionID)
	c.mu.Unlock()
	if s != nil {
		s.close()
	}
}

// Close tears down every conversation's process and the shared aggregator.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	sessions := c.sessions
	c.sessions = make(map[string]*acpSession)
	c.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
	// The aggregator is shared across the agent's sessions; close its proxied MCP
	// connections once, here, not per session.
	if closer, ok := c.opts.Aggregator.(io.Closer); ok {
		_ = closer.Close()
	}
	return nil
}

// Capabilities returns what the agent advertised at initialize. Zero value until
// Initialize completes.
func (c *Client) Capabilities() AgentCapabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caps
}

// SupportsCancel reports the interruptibility resolved at Initialize. Until then
// it returns true so a caller never disables interrupts on an unresolved probe.
func (c *Client) SupportsCancel(context.Context) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.probed {
		return true
	}
	return c.interruptible
}
