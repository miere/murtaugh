package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

// subscription is a single prompt turn's event stream plus a drain barrier. The
// readLoop is a long-lived sender (session/update notifications and
// agent-initiated permission asks) that cannot be stopped per-turn the way the
// heartbeat can; wg counts its in-flight sends into events so teardown can wait
// for them to drain before closing the channel. Without it, a trailing
// notification arriving as a turn tears down sends on a closed channel and the
// process panics.
type subscription struct {
	events chan agent.Event
	wg     sync.WaitGroup
}

// promptScope is the in-flight context for the session's current prompt: where it
// is talking (loc), the context that is cancelled when the turn ends, and whether
// any reply text was already streamed this turn (sawText) so the final result
// payload is not re-emitted on top of the streamed chunks.
type promptScope struct {
	loc     agent.TurnLocation
	ctx     context.Context
	sawText *atomic.Bool
}

// acpSession is one ACP agent process bound to a single conversation: it owns the
// child process, the JSON-RPC transport, and exactly one ACP session (session/new).
// The manager (Client) runs one of these per Slack conversation, which is what
// gives each conversation an isolated process — no cross-conversation taint, no
// shared-process concurrency. Because there is only ever one turn in flight per
// session, the former per-session maps collapse to the single active/scope/watcher
// fields here.
type acpSession struct {
	opts ProcessOptions
	log  *slog.Logger
	// now sources the current time for the per-turn <context> block. Injectable
	// so tests can assert a fixed timestamp; defaults to time.Now.
	now func() time.Time

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	started bool
	closed  bool
	nextID  atomic.Int64
	pending map[int64]chan rpcResponse
	// active is the current turn's subscription; nil between turns.
	active *subscription
	// scope is where the current turn is talking and its cancellable context;
	// zero between turns. Written under mu by prompt, read by deliverNotification
	// and the permission handler.
	scope promptScope
	// watcher feeds the current turn's heartbeat/ceiling; nil between turns.
	watcher *toolWatcher
	// sessionID is the agent-assigned id from session/new.
	sessionID string
	// release drops this session's aggregator registration; nil when there is no
	// aggregator. Run once on close.
	release func()
}

func newACPSession(opts ProcessOptions) *acpSession {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &acpSession{opts: opts, log: logger, now: time.Now, pending: make(map[int64]chan rpcResponse)}
}

// bringUp starts the process and performs the ACP initialize handshake, returning
// the agent's advertised capabilities. It does NOT open a session — the manager
// calls openSession next (or, for the startup probe, skips it).
func (c *acpSession) bringUp(ctx context.Context) (AgentCapabilities, error) {
	if err := c.start(ctx); err != nil {
		return AgentCapabilities{}, err
	}
	result, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientInfo": map[string]any{
			"name":    "murtaugh",
			"title":   "Murtaugh Slack ACP Client",
			"version": "0.1.0",
		},
		// We service the agent-initiated filesystem methods (fs.go), so advertise
		// them. Without this, claude-agent-acp's built-in Read/Write tools route
		// fs/read_text_file to us and block forever when we don't answer — the
		// agent goes silent mid-turn and trips the idle watchdog.
		"clientCapabilities": map[string]any{
			"fs": map[string]any{"readTextFile": true, "writeTextFile": true},
		},
	})
	if err != nil {
		return AgentCapabilities{}, err
	}
	return parseAgentCapabilities(result), nil
}

func (c *acpSession) start(ctx context.Context) error {
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
	// Inherit Murtaugh's environment (minus the nested-Claude-Code marker — see
	// agent.SpawnEnv) plus the profile's overrides.
	cmd.Env = agent.SpawnEnv(c.opts.Env)
	// Run in a dedicated process group so close() can tear down the adapter AND
	// the grandchildren it spawns (the mcp-bridge, the claude CLI, proxied MCP
	// servers) rather than orphaning them on shutdown/restart.
	agent.SetProcessGroup(cmd)
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

func (c *acpSession) markProcessExited(cmd *exec.Cmd) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != cmd {
		return
	}
	c.started = false
	c.stdin = nil
	c.cmd = nil
}

// openSession issues session/new (advertising the aggregator's MCP server, if
// any) and binds the returned id to this session. It records the aggregator
// release so close can drop the registration.
func (c *acpSession) openSession(ctx context.Context, meta agent.SessionMetadata) (string, error) {
	mcpServers, release := c.aggregatorServers(meta)
	result, err := c.call(ctx, "session/new", map[string]any{
		"cwd":        c.sessionCWD(),
		"mcpServers": mcpServers,
	})
	if err != nil {
		if release != nil {
			release()
		}
		return "", err
	}
	var decoded struct {
		SessionID string `json:"sessionId"`
		ID        string `json:"id"`
	}
	if len(result) > 0 {
		if err := json.Unmarshal(result, &decoded); err != nil {
			if release != nil {
				release()
			}
			return "", fmt.Errorf("decode session/new response: %w", err)
		}
	}
	id := decoded.SessionID
	if id == "" {
		id = decoded.ID
	}
	if id == "" {
		if release != nil {
			release()
		}
		return "", errors.New("session/new response did not include sessionId")
	}
	c.mu.Lock()
	c.sessionID = id
	c.release = release
	c.mu.Unlock()
	return id, nil
}

// aggregatorServers asks the aggregator (if any) to register this session and
// returns the mcpServers value for session/new plus a release to run if the
// session fails to open. An empty list (and nil release) when no aggregator is
// configured or registration fails — the agent then simply gets no Murtaugh
// tools, logged loudly rather than failing the session.
func (c *acpSession) aggregatorServers(meta agent.SessionMetadata) ([]any, func()) {
	if c.opts.Aggregator == nil {
		return []any{}, nil
	}
	spec, release, err := c.opts.Aggregator.RegisterSession(meta)
	if err != nil {
		c.log.Warn("aggregator registration failed; ACP agent will have no Murtaugh tools", "error", err)
		return []any{}, nil
	}
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, map[string]string{"name": k, "value": spec.Env[k]})
	}
	server := map[string]any{
		"name":    spec.Name,
		"command": spec.Command,
		"args":    spec.Args,
		"env":     env,
	}
	return []any{server}, release
}

func (c *acpSession) sessionCWD() string {
	if strings.TrimSpace(c.opts.WorkDir) != "" {
		return c.opts.WorkDir
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// cancelTurn asks the agent to abort this session's in-flight prompt.
func (c *acpSession) cancelTurn(ctx context.Context) error {
	_, err := c.call(ctx, "session/cancel", map[string]any{"sessionId": c.sessionID})
	return err
}

// supportsCancel probes whether the agent implements session/cancel by issuing
// the call for a synthetic session id and inspecting the outcome. A method-not-
// found error means the agent cannot be interrupted; any other result means the
// method exists. A transient failure conservatively reports true so a flaky probe
// never silently disables interrupts.
func (c *acpSession) supportsCancel(ctx context.Context) bool {
	_, err := c.call(ctx, "session/cancel", map[string]any{"sessionId": cancelProbeSessionID})
	return !IsMethodNotFound(err)
}

// close tears down the process and drops this session's aggregator registration.
// It does NOT close the shared aggregator's proxied connections — the manager
// owns that (there is one aggregator across all of an agent's sessions).
func (c *acpSession) close() {
	c.mu.Lock()
	c.closed = true
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	// Kill the whole process group (adapter + mcp-bridge + claude + MCP servers),
	// not just the adapter, so nothing is orphaned.
	agent.KillProcessGroup(c.cmd)
	release := c.release
	c.release = nil
	c.mu.Unlock()
	if release != nil {
		release()
	}
	c.failAll(errors.New("ACP session closed"))
}
