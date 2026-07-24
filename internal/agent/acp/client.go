package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

type ProcessOptions struct {
	Command string
	Args    []string
	WorkDir string
	// Env are extra KEY=VALUE entries layered on top of the inherited
	// environment when the agent process is started. Empty leaves the process
	// with Murtaugh's own environment unchanged.
	Env    []string
	Logger *slog.Logger

	// PermissionPolicy governs how agent-initiated session/request_permission
	// requests are answered: "ask" (raise an agent.EventPermission on the turn's event
	// stream for the consumer to resolve with a human), "auto-allow", or
	// "auto-deny". Empty is treated as "ask". "ask" with no live turn consuming
	// events (headless/CLI) denies — fail-safe and fast, never a hang.
	PermissionPolicy string
	// Aggregator, when set, registers each session with Murtaugh's per-agent MCP
	// aggregator and supplies the stdio bridge server advertised in session/new,
	// so the agent can reach Murtaugh's own tools. nil leaves mcpServers empty.
	Aggregator agent.Aggregator
	// Persona is Murtaugh's shared persona (SOUL.md). ACP exposes no system role,
	// so when set it is injected as a leading <persona> block on every prompt,
	// keeping an ACP agent's voice aligned with native — even when the agent runs
	// in an external project with its own AGENTS.md. Empty injects nothing.
	Persona string
	// ToolHeartbeatInterval is how often a still-running tool emits a keep-alive
	// status event so the gateway's idle watchdog does not treat a long,
	// output-silent tool call as a stall. Zero takes defaultACPToolHeartbeatInterval.
	ToolHeartbeatInterval time.Duration
	// ToolCeiling bounds how long a single tool call may hold a turn before it is
	// failed with ErrToolCeiling; the heartbeat suppresses the idle watchdog while a
	// tool runs, so this is what stops a wedged tool. Zero takes
	// defaultACPToolCeiling; a negative value disables the ceiling.
	ToolCeiling time.Duration
}

// subscription is a single prompt turn's event stream plus a drain barrier. The
// readLoop is a long-lived, session-shared sender (session/update notifications
// and agent-initiated permission asks) that cannot be stopped per-turn the way
// the heartbeat can; wg counts its in-flight sends into events so teardown can
// wait for them to drain before closing the channel. Without it, a trailing
// notification arriving as a turn tears down sends on a closed channel and the
// process panics.
type subscription struct {
	events chan agent.Event
	wg     sync.WaitGroup
}

type ProcessClient struct {
	opts ProcessOptions
	log  *slog.Logger
	// now sources the current time for the per-turn <context> block. Injectable
	// so tests can assert a fixed timestamp; defaults to time.Now.
	now func() time.Time

	mu          sync.Mutex
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	started     bool
	closed      bool
	nextID      atomic.Int64
	pending     map[int64]chan rpcResponse
	subscribers map[string]*subscription
	// dests records, per active session, the Slack conversation and the prompt's
	// context so an agent-initiated session/request_permission can be routed to a
	// human in the right thread and cancelled when that turn is interrupted.
	dests map[string]promptScope
	// toolWatch records, per active session, the in-flight tool set feeding that
	// turn's heartbeat/ceiling. Written by deliverNotification (readLoop) and read
	// by the heartbeat goroutine; guarded by mu.
	toolWatch map[string]*toolWatcher
	// caps records what the agent advertised in its initialize response. Set once
	// by Initialize before any prompt runs, then read-only; guarded by mu.
	caps AgentCapabilities
	// releases holds each registered session's aggregator cleanup, run on Close.
	releases []func()
}

func NewProcessClient(opts ProcessOptions) *ProcessClient {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &ProcessClient{opts: opts, log: logger, now: time.Now, pending: make(map[int64]chan rpcResponse), subscribers: make(map[string]*subscription), dests: make(map[string]promptScope), toolWatch: make(map[string]*toolWatcher)}
}

func (c *ProcessClient) Initialize(ctx context.Context) error {
	startedAt := time.Now()
	if err := c.start(ctx); err != nil {
		return err
	}
	result, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientInfo": map[string]any{
			"name":    "murtaugh",
			"title":   "Murtaugh Slack ACP Client",
			"version": "0.1.0",
		},
		// We service the agent-initiated filesystem methods below, so advertise
		// them. Without this, claude-agent-acp's built-in "acp" Read/Write/Edit
		// tools route fs/read_text_file to us and block forever when we don't
		// answer — the agent goes silent mid-turn and trips the idle watchdog.
		"clientCapabilities": map[string]any{
			"fs": map[string]any{
				"readTextFile":  true,
				"writeTextFile": true,
			},
		},
	})
	if err != nil {
		return err
	}
	caps := parseAgentCapabilities(result)
	c.mu.Lock()
	c.caps = caps
	c.mu.Unlock()
	c.log.Info("initialized ACP client",
		"duration", time.Since(startedAt),
		"protocol_version", caps.ProtocolVersion,
		"mcp_http", caps.MCP.HTTP,
		"mcp_sse", caps.MCP.SSE,
		"load_session", caps.LoadSession,
	)
	return nil
}

// unsubscribe retracts a prompt's event subscription, but only if it is still
// the live one. When two prompts race on the same session (e.g. an interrupt
// immediately followed by a follow-up that reuses the session), the second
// prompt overwrites subscribers[sessionID]; an unconditional delete here would
// tear down the live prompt's subscription and silently drop its events.
func (c *ProcessClient) unsubscribe(sessionID string, sub *subscription) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.subscribers[sessionID] == sub {
		delete(c.subscribers, sessionID)
		delete(c.dests, sessionID)
	}
}

// closeSubscription retracts a prompt's subscription and closes its event
// channel — but only after every readLoop-originated send that already captured
// this subscription has drained. The readLoop looks up the subscription under
// the lock and sends without it (the send is deliberately blocking, for
// back-pressure); retracting the map entry under the lock stops NEW sends, and
// wg.Wait then waits out the ones already past the lookup. Closing before that
// drain is what let a trailing notification panic on a closed channel. The
// heartbeat, the other sender, is already stopped and awaited by the caller.
//
// wg is bound to sub, not to sessionID, so an interrupt-then-followup that
// overwrote subscribers[sessionID] still drains and closes THIS turn's channel
// rather than the live one's.
func (c *ProcessClient) closeSubscription(sessionID string, sub *subscription) {
	c.unsubscribe(sessionID, sub)
	sub.wg.Wait()
	close(sub.events)
}

// clearToolWatch retracts a prompt's tool watcher, but only if it is still the
// live one — a racing follow-up prompt on the same session installs its own, and
// an unconditional delete would blind that turn's heartbeat.
func (c *ProcessClient) clearToolWatch(sessionID string, w *toolWatcher) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.toolWatch[sessionID] == w {
		delete(c.toolWatch, sessionID)
	}
}

func (c *ProcessClient) Cancel(ctx context.Context, sessionID string) error {
	_, err := c.call(ctx, "session/cancel", map[string]any{"sessionId": sessionID})
	return err
}

// SupportsCancel probes whether the agent implements session/cancel by issuing
// the call for a synthetic session and inspecting the outcome. A method-not-
// found error means the agent cannot be interrupted; any other result (success
// or an unknown-session error) means the method exists. On a transient/ambient
// failure (process error, cancelled context) it conservatively reports true so
// a flaky probe never silently disables interrupts.
func (c *ProcessClient) SupportsCancel(ctx context.Context) bool {
	err := c.Cancel(ctx, cancelProbeSessionID)
	return !IsMethodNotFound(err)
}

func (c *ProcessClient) Close() error {
	c.mu.Lock()
	c.closed = true
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	releases := c.releases
	c.releases = nil
	c.mu.Unlock()
	// Drop every aggregator session this client registered so its tokens can no
	// longer be claimed.
	for _, release := range releases {
		release()
	}
	// Tear down the aggregator's proxied MCP connections, if it holds any.
	if closer, ok := c.opts.Aggregator.(io.Closer); ok {
		_ = closer.Close()
	}
	c.failAll(errors.New("ACP client closed"))
	return nil
}

func (c *ProcessClient) start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return nil
	}
	if strings.TrimSpace(c.opts.Command) == "" {
		return errors.New("ACP command is required")
	}
	cmd := exec.Command(c.opts.Command, c.opts.Args...)
	cmd.Dir = c.opts.WorkDir
	if len(c.opts.Env) > 0 {
		// Inherit Murtaugh's environment, then append the profile's overrides.
		// exec resolves a duplicate key to the last entry, so appending the
		// overrides last makes them win over an inherited var of the same name.
		cmd.Env = append(os.Environ(), c.opts.Env...)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open ACP stdout: %w", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open ACP stdin: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("open ACP stderr: %w", err)
	}
	c.log.Info("starting ACP process", "command", c.opts.Command, "workdir", c.opts.WorkDir)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ACP process: %w", err)
	}
	c.cmd = cmd
	c.stdin = stdin
	c.started = true
	go c.readLoop(stdout)
	go c.drainStderr(stderr)
	go func() {
		err := cmd.Wait()
		c.markProcessExited(cmd)
		c.log.Info("ACP process exited", "error", err)
		c.failAll(errors.New("ACP process exited"))
	}()
	return nil
}

func (c *ProcessClient) markProcessExited(cmd *exec.Cmd) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != cmd {
		return
	}
	c.started = false
	c.stdin = nil
	c.cmd = nil
}
