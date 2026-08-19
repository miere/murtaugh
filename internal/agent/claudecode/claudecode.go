package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

// defaultArgs launches `claude` in headless bidirectional stream-json mode: a
// long-lived process that reads NDJSON user turns on stdin and streams NDJSON
// events on stdout. --verbose is required with --output-format=stream-json.
//
// `--permission-prompt-tool stdio` is the enabler for the control-protocol
// permission route: it is a reserved sentinel (not an MCP tool name) that tells
// the CLI to ask the controlling process for tool permission via a can_use_tool
// control_request instead of auto-denying. Verified against 2.1.216 — without it
// a headless turn silently denies every gated tool (spec 019 §6).
//
// `--disallowedTools AskUserQuestion` removes Claude Code's own
// question-asking built-in. It renders in the terminal UI, which a headless
// session does not have, so every call to it fails and the model is left
// unable to ask anything. Murtaugh publishes a replacement over MCP under the
// same name (internal/tools/ask, via MCPName), so hiding the built-in is what
// makes the model reach for the one that works — with the payload it already
// knows. Without this flag the built-in shadows it.
var defaultArgs = []string{
	"-p",
	"--input-format", "stream-json",
	"--output-format", "stream-json",
	"--verbose",
	"--permission-prompt-tool", "stdio",
	"--disallowedTools", "AskUserQuestion",
}

// Options configures a Client. Command is required. Args defaults to defaultArgs
// (the stream-json launch) when nil; tests inject a fake process via Command/Args.
type Options struct {
	Command string
	Args    []string
	// Model, when set, is appended as `--model <Model>` to the launch args.
	Model   string
	Env     []string
	WorkDir string
	Logger  *slog.Logger
	// PermissionPolicy governs a can_use_tool request: "ask" (default — route to a
	// human in Slack by raising an EventPermission on the turn, exactly like the
	// ACP path), "auto-allow", or "auto-deny". Headless turns (no active
	// subscriber) always deny under "ask" — fail-safe, never a hang.
	PermissionPolicy string
	// OnBackground receives events a session emits with no active turn — chiefly a
	// background subagent's post-`result` completion and the model's auto-continue.
	// It is keyed by session id so the gateway can render them into the bound Slack
	// thread (spec 019 §5). nil drops them (with a debug log).
	OnBackground func(sessionID string, ev agent.Event)
	// Aggregator, when set, registers each session with Murtaugh's per-agent MCP
	// aggregator and advertises the stdio bridge to the `claude` process via
	// --mcp-config, so the agent can reach Murtaugh's own tools — the same tool
	// surface the ACP and native backends expose. nil leaves the process with only
	// the tools the claude CLI configures itself.
	Aggregator agent.Aggregator
	// Sandbox confines the spawned process and every descendant it forks (node,
	// git, ripgrep, the mcp-bridge grandchild). nil (the default) spawns
	// unconfined, exactly as before.
	Sandbox agent.Sandbox
	// ToolHeartbeatInterval is how often a still-running tool emits a keep-alive
	// status event, so the gateway's idle watchdog does not read an output-silent
	// tool as a stalled turn. Zero takes agent.DefaultToolHeartbeatInterval.
	ToolHeartbeatInterval time.Duration
	// ToolCeiling bounds how long one tool may hold a turn; past it the turn is
	// failed with agent.ErrToolCeiling. Zero takes agent.DefaultToolCeiling; a
	// negative value disables the ceiling. Mirrors the ACP option of the same name.
	ToolCeiling time.Duration
	// Now is the clock the tool watcher ages tools against. nil uses time.Now;
	// tests inject a fake so the ceiling can be driven without waiting on one.
	Now func() time.Time
}

// Client is a Claude Code stream-json backend implementing agent.Client. Because
// a `claude` stream-json process is bound to a single session (--session-id is a
// launch arg), the client multiplexes conversations by running ONE process per
// session, keyed by the deterministic session id. The gateway shares one Client
// across every thread routed to an agent, so this multiplexing is what lets
// concurrent Slack conversations coexist.
//
// Each session process is held open across turns, so a background subagent
// completing after a turn's `result` still reaches us — routed via OnBackground.
type Client struct {
	opts Options
	log  *slog.Logger

	mu       sync.Mutex
	closed   bool
	sessions map[string]*procSession
}

// New builds a Client. It does not start any process; call Initialize then
// NewSession (which starts a per-session process).
func New(opts Options) *Client {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Args == nil {
		opts.Args = defaultArgs
	}
	if opts.Model != "" {
		opts.Args = append(append([]string{}, opts.Args...), "--model", opts.Model)
	}
	return &Client{opts: opts, log: opts.Logger, sessions: make(map[string]*procSession)}
}

// mcpConfigArg renders an aggregator MCP server spec into the JSON the `claude`
// CLI accepts via --mcp-config: {"mcpServers":{"<name>":{command,args,env}}}. The
// bridge command is stdio (command-based), so no transport type is needed.
func mcpConfigArg(spec agent.MCPServerSpec) (string, error) {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			spec.Name: map[string]any{
				"command": spec.Command,
				"args":    spec.Args,
				"env":     spec.Env,
			},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("claudecode: marshal mcp config: %w", err)
	}
	return string(b), nil
}

// Initialize validates the client is ready. Per-session processes are started
// lazily in NewSession, where the conversation metadata (and thus the derived
// --session-id) is known.
func (c *Client) Initialize(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("claudecode: client is closed")
	}
	if strings.TrimSpace(c.opts.Command) == "" {
		return errors.New("claudecode: command is required")
	}
	return nil
}

// NewSession derives the deterministic session id from the Slack conversation and
// starts (or reuses) the process bound to it. The returned id is authoritative.
func (c *Client) NewSession(ctx context.Context, meta agent.SessionMetadata) (agent.Session, error) {
	sessionID := agent.DeriveSessionID(meta)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return agent.Session{}, errors.New("claudecode: client is closed")
	}
	if sess, ok := c.sessions[sessionID]; ok {
		c.mu.Unlock()
		return agent.Session{ID: sess.id}, nil
	}
	c.mu.Unlock()

	sess := newProcSession(sessionID, c.opts)
	// Register this session with Murtaugh's tool aggregator (if wired) and hand the
	// `claude` process the stdio bridge via --mcp-config, so it can reach Murtaugh's
	// own tools (slack.*, jobs, …) — parity with the ACP/native backends. A failure
	// here degrades to no Murtaugh tools rather than failing the turn.
	if c.opts.Aggregator != nil {
		spec, release, err := c.opts.Aggregator.RegisterSession(meta)
		if err != nil {
			c.log.Warn("claudecode: aggregator registration failed; agent will have no Murtaugh tools", "session", sessionID, "error", err)
		} else if cfg, cerr := mcpConfigArg(spec); cerr != nil {
			release()
			c.log.Warn("claudecode: failed to build mcp config; agent will have no Murtaugh tools", "session", sessionID, "error", cerr)
		} else {
			sess.extraArgs = []string{"--mcp-config", cfg}
			sess.releaseAgg = release
		}
	}
	if err := sess.startSession(ctx); err != nil {
		if sess.releaseAgg != nil {
			sess.releaseAgg()
		}
		return agent.Session{}, err
	}
	c.mu.Lock()
	// A concurrent NewSession may have won the race; prefer the stored one.
	if existing, ok := c.sessions[sessionID]; ok {
		c.mu.Unlock()
		_ = sess.close()
		return agent.Session{ID: existing.id}, nil
	}
	c.sessions[sessionID] = sess
	c.mu.Unlock()
	return agent.Session{ID: sessionID}, nil
}

// Prompt sends one user turn to the identified session and returns its event
// stream. The channel closes when the turn's `result` arrives; the process stays
// alive for the next turn and for any background completion.
func (c *Client) Prompt(_ context.Context, sessionID string, req agent.PromptRequest) (<-chan agent.Event, error) {
	c.mu.Lock()
	sess, ok := c.sessions[sessionID]
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("claudecode: no session %q", sessionID)
	}
	return sess.prompt(req)
}

// Cancel interrupts the identified session's in-flight turn via the control
// channel. The session survives; background subagents keep running.
func (c *Client) Cancel(ctx context.Context, sessionID string) error {
	c.mu.Lock()
	sess, ok := c.sessions[sessionID]
	c.mu.Unlock()
	if !ok {
		return nil // nothing in flight
	}
	return sess.cancel(ctx)
}

// CloseSession tears down one session's process, e.g. when the gateway evicts an
// idle conversation. Unknown ids are ignored.
func (c *Client) CloseSession(sessionID string) {
	c.mu.Lock()
	sess := c.sessions[sessionID]
	delete(c.sessions, sessionID)
	c.mu.Unlock()
	if sess != nil {
		_ = sess.close()
	}
}

// Close terminates every session process. Safe to call more than once.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	sessions := c.sessions
	c.sessions = make(map[string]*procSession)
	c.mu.Unlock()
	for _, sess := range sessions {
		_ = sess.close()
	}
	// Tear down the aggregator's proxied MCP connections, mirroring the ACP client.
	if closer, ok := c.opts.Aggregator.(io.Closer); ok {
		_ = closer.Close()
	}
	return nil
}

// --- procSession: one process == one session ------------------------------

// subscription is one open turn: the event channel handed to the caller, plus the
// tool bookkeeping that keeps the turn alive while a tool runs (see heartbeat.go).
type subscription struct {
	events chan agent.Event

	// watcher tracks the turn's in-flight tool calls, fed from the same task
	// events the caller sees. hbStop/hbDone/hbOnce are the heartbeat's shutdown
	// handshake; nil hbStop means no heartbeat was started.
	watcher *agent.ToolWatcher
	hbStop  chan struct{}
	hbDone  chan struct{}
	hbOnce  sync.Once
}

type procSession struct {
	id     string
	log    *slog.Logger
	opts   Options // shared launch config + policy + OnBackground
	policy string
	// extraArgs are launch args added to every spawn of this session — currently
	// the --mcp-config that advertises Murtaugh's tool bridge (set in NewSession).
	extraArgs []string
	// releaseAgg unregisters this session's aggregator token; called on close.
	releaseAgg func()

	mu       sync.Mutex
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stderr   *cappedBuffer
	procDone chan struct{}
	closed   bool

	active  *subscription
	pending map[string]chan *streamMessage
	reqSeq  atomic.Int64
	// now is the session's clock, resolved once from Options.Now.
	now func() time.Time
}

func newProcSession(id string, opts Options) *procSession {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &procSession{
		id:      id,
		log:     opts.Logger.With("session", id),
		opts:    opts,
		policy:  strings.ToLower(strings.TrimSpace(opts.PermissionPolicy)),
		pending: make(map[string]chan *streamMessage),
		now:     now,
	}
}

// startSession brings up the process, preferring to resume an existing on-disk
// session and falling back to creating a fresh one (--session-id is create-only).
func (s *procSession) startSession(ctx context.Context) error {
	if err := s.spawnAndHandshake(ctx, []string{"--resume", s.id}); err == nil {
		return nil
	} else {
		s.log.Debug("claudecode: resume failed; creating fresh session", "error", err)
		resumeErr := err
		s.teardownProc()
		if createErr := s.spawnAndHandshake(ctx, []string{"--session-id", s.id}); createErr != nil {
			return fmt.Errorf("claudecode: start session %s: resume=%v; create=%w", s.id, resumeErr, createErr)
		}
		return nil
	}
}

func (s *procSession) spawnAndHandshake(ctx context.Context, sessionArgs []string) error {
	if err := s.start(sessionArgs); err != nil {
		return err
	}
	if err := s.handshake(ctx); err != nil {
		return fmt.Errorf("%w%s", err, s.stderrSuffix())
	}
	return nil
}

func (s *procSession) start(sessionArgs []string) error {
	args := append(append([]string{}, s.opts.Args...), s.extraArgs...)
	args = append(args, sessionArgs...)
	// Apply confinement HERE, after every argument layer is settled — never at the
	// build seam. New() falls back to defaultArgs only when Args is empty and then
	// appends --model to it; wrapping earlier would make Args non-empty and
	// silently eat the entire stream-json launch. A nil Sandbox is a no-op.
	command, args := agent.WrapCommand(s.opts.Sandbox, s.opts.Command, args)
	cmd := exec.Command(command, args...)
	cmd.Dir = s.opts.WorkDir
	// Inherit the daemon's environment minus the nested-Claude-Code marker (so the
	// claude CLI launches even when Murtaugh itself runs inside a Claude Code
	// session), plus the profile's overrides. Under a sandbox the inherited set is
	// first reduced to that box's allowlist.
	cmd.Env = agent.SpawnEnvFor(s.opts.Sandbox, s.opts.Env)
	// Run in a dedicated process group so teardown kills the claude CLI AND the
	// MCP servers it spawns (including Murtaugh's own mcp-bridge) rather than
	// orphaning them on shutdown/restart.
	agent.SetProcessGroup(cmd)
	stderr := &cappedBuffer{limit: 8 << 10}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("claudecode: open stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("claudecode: open stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("claudecode: start %q: %w", s.opts.Command, err)
	}
	done := make(chan struct{})
	s.mu.Lock()
	s.cmd = cmd
	s.stdin = stdin
	s.stderr = stderr
	s.procDone = done
	s.mu.Unlock()
	go func() {
		s.readLoop(stdout)
		_ = cmd.Wait()
		close(done)
	}()
	return nil
}

func (s *procSession) handshake(ctx context.Context) error {
	resp, err := s.sendControl(ctx, map[string]any{"subtype": "initialize"})
	if err != nil {
		return fmt.Errorf("claudecode: initialize handshake: %w", err)
	}
	if controlResponseSubtype(resp) == "error" {
		return fmt.Errorf("claudecode: initialize rejected: %s", controlResponseError(resp))
	}
	return nil
}

func (s *procSession) teardownProc() {
	s.mu.Lock()
	stdin, cmd := s.stdin, s.cmd
	s.stdin, s.cmd = nil, nil
	s.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	// Kill the whole process group (claude CLI + the MCP servers it spawned,
	// including Murtaugh's mcp-bridge), not just the direct process.
	agent.KillProcessGroup(cmd)
}

// composePrompt folds any backfilled history (thread transcript, canvas context)
// into the first user turn. A cold claude_code session has no other channel for
// it — the ACP and native backends prepend it the same way (acp/prompt.go,
// native/client.go). Without this the model sees only the bare prompt and answers
// with no context (e.g. no awareness it was mentioned inside a canvas).
func composePrompt(req agent.PromptRequest) string {
	if h := strings.TrimSpace(req.History); h != "" {
		return h + "\n\n" + req.Text
	}
	return req.Text
}

func (s *procSession) prompt(req agent.PromptRequest) (<-chan agent.Event, error) {
	sub := &subscription{
		events:  make(chan agent.Event, 64),
		watcher: agent.NewToolWatcher(s.now),
		hbStop:  make(chan struct{}),
		hbDone:  make(chan struct{}),
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("claudecode: session is closed")
	}
	s.active = sub
	s.mu.Unlock()

	if err := s.writeUser(composePrompt(req)); err != nil {
		s.mu.Lock()
		s.active = nil
		s.mu.Unlock()
		// The heartbeat never started, so close its done channel to keep the
		// subscription's invariant (hbDone is always closed once the turn is over)
		// rather than leaving a later stopHeartbeat to block on it.
		close(sub.hbDone)
		close(sub.events)
		return nil, err
	}
	go s.heartbeat(sub)
	return sub.events, nil
}

func (s *procSession) cancel(ctx context.Context) error {
	_, err := s.sendControl(ctx, map[string]any{"subtype": "interrupt"})
	return err
}

func (s *procSession) close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	stdin, cmd := s.stdin, s.cmd
	release := s.releaseAgg
	s.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	// Kill the whole process group (claude CLI + the MCP servers it spawned,
	// including Murtaugh's mcp-bridge), not just the direct process.
	agent.KillProcessGroup(cmd)
	if release != nil {
		release()
	}
	return nil
}

// --- stream reading -------------------------------------------------------

func (s *procSession) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		msg, err := decodeMessage(scanner.Bytes())
		if err != nil {
			s.log.Warn("claudecode: undecodable stream line", "error", err)
			continue
		}
		if msg == nil {
			continue
		}
		s.dispatch(msg)
	}
	s.failActive(errors.New("claudecode: stream closed"))
}

func (s *procSession) dispatch(msg *streamMessage) {
	switch {
	case msg.Type == "control_response":
		s.deliverControlResponse(msg)
	case msg.Type == "control_request":
		go s.handleControlRequest(msg)
	case msg.isResult():
		stop := msg.StopReason
		if stop == "" {
			stop = "end_turn"
		}
		s.completeActive(stop)
	default:
		for _, ev := range msg.toEvents() {
			s.emit(ev)
		}
	}
}

// emit delivers to the active turn, or — when a turn is not open — to the
// background sink (a subagent completion arriving after `result`).
func (s *procSession) emit(ev agent.Event) {
	s.mu.Lock()
	sub := s.active
	s.mu.Unlock()
	if sub != nil {
		// Fold task events into the turn's watcher before delivering them. A
		// `tool_use` block opens a tool and the matching `tool_result` retires it,
		// which is the only signal the stream gives that a silent stretch is a
		// tool running rather than the agent stalling.
		if ev.Type == agent.EventTask && ev.Task != nil && sub.watcher != nil {
			sub.watcher.Observe(ev.Task.ID, ev.Task.Title, ev.Task.Status)
		}
		sub.events <- ev
		return
	}
	if s.opts.OnBackground != nil {
		s.opts.OnBackground(s.id, ev)
		return
	}
	s.log.Debug("claudecode: dropping background event (no active turn, no sink)", "type", string(ev.Type))
}

func (s *procSession) completeActive(stopReason string) {
	s.mu.Lock()
	sub := s.active
	s.active = nil
	s.mu.Unlock()
	if sub == nil {
		// A `result` with no active turn closes a background auto-continue: signal
		// it to the sink so the gateway can finalise the thread message.
		if s.opts.OnBackground != nil {
			s.opts.OnBackground(s.id, agent.Event{Type: agent.EventComplete, StopReason: stopReason})
		}
		return
	}
	// Stop the heartbeat and wait for it before touching the channel: it is the
	// other writer, and closing underneath it would panic on a send.
	sub.stopHeartbeat()
	sub.events <- agent.Event{Type: agent.EventComplete, StopReason: stopReason}
	close(sub.events)
}

func (s *procSession) failActive(err error) {
	s.mu.Lock()
	sub := s.active
	s.active = nil
	s.mu.Unlock()
	if sub == nil {
		return
	}
	sub.stopHeartbeat()
	sub.events <- agent.Event{Type: agent.EventError, Error: err}
	close(sub.events)
}

// --- control protocol -----------------------------------------------------

func (s *procSession) sendControl(ctx context.Context, request map[string]any) (*streamMessage, error) {
	id := fmt.Sprintf("req-%d", s.reqSeq.Add(1))
	ch := make(chan *streamMessage, 1)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("claudecode: session is closed")
	}
	done := s.procDone
	s.pending[id] = ch
	s.mu.Unlock()
	if done == nil {
		done = make(chan struct{})
	}

	frame := map[string]any{"type": "control_request", "request_id": id, "request": request}
	if err := s.writeJSON(frame); err != nil {
		s.dropPending(id)
		return nil, err
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-done:
		s.dropPending(id)
		return nil, errors.New("claudecode: process exited before control response")
	case <-ctx.Done():
		s.dropPending(id)
		return nil, ctx.Err()
	}
}

func (s *procSession) dropPending(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
}

func (s *procSession) deliverControlResponse(msg *streamMessage) {
	id := controlResponseRequestID(msg)
	if id == "" {
		return
	}
	s.mu.Lock()
	ch := s.pending[id]
	delete(s.pending, id)
	s.mu.Unlock()
	if ch != nil {
		ch <- msg
	}
}

func (s *procSession) handleControlRequest(msg *streamMessage) {
	sub := controlRequestSubtype(msg)
	reqID := rawString(msg.RequestID)
	switch sub {
	case "can_use_tool":
		s.answerPermission(reqID, msg)
	default:
		s.writeControlResponseError(reqID, fmt.Sprintf("claudecode: unsupported control request %q", sub))
	}
}

// denyFallback is used when a deny path reports no reason of its own. The CLI
// rejects a deny carrying no message at all, so there is always something to say.
const denyFallback = "Murtaugh denied this tool call."

func (s *procSession) answerPermission(reqID string, msg *streamMessage) {
	toolName, input := parseCanUseTool(msg.Request)
	allow, reason := s.decidePermission(toolName, input)
	var inner map[string]any
	if allow {
		inner = map[string]any{"behavior": "allow"}
		if len(input) > 0 {
			inner["updatedInput"] = json.RawMessage(input)
		}
	} else {
		// A deny MUST carry a message. The CLI validates the shape and rejects a
		// message-less deny outright ("The canUseTool callback returned an invalid
		// permission result"), which fails the whole turn rather than just the one
		// tool call. The message is model-facing — it becomes the tool_result the
		// model reads — so it explains why instead of only saying no.
		if strings.TrimSpace(reason) == "" {
			reason = denyFallback
		}
		inner = map[string]any{"behavior": "deny", "message": reason}
	}
	s.writeJSON(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": reqID,
			"response":   inner,
		},
	})
}

// decidePermission resolves a can_use_tool request. auto-allow/auto-deny answer
// without a human; the default ("ask") raises an EventPermission on the active
// turn — reusing the chat handler's approval card exactly like the ACP path — and
// blocks on the human's decision. No active turn or a dead process denies.
//
// Every deny returns a reason alongside it. The reason is not diagnostics: it is
// handed to the model as the denied call's result, so each path says which of the
// several quite different denials this was — a policy, a missing conversation, or
// an actual human saying no.
func (s *procSession) decidePermission(toolName string, input json.RawMessage) (bool, string) {
	switch s.policy {
	case "auto-allow":
		return true, ""
	case "auto-deny":
		return false, "Murtaugh is configured to deny every tool call in this session."
	default: // ask
		s.mu.Lock()
		sub := s.active
		done := s.procDone
		s.mu.Unlock()
		if sub == nil {
			s.log.Warn("claudecode: permission request with no active turn; denying", "tool", toolName)
			return false, "There is no active Slack conversation to ask for approval in, so this call was denied. Do not retry it."
		}
		decision := make(chan string, 1)
		// No options of our own: can_use_tool is a bare allow/deny, so the whole
		// decision is Murtaugh's, and it supplies the buttons it wants to offer —
		// including "always allow", which a hardcoded pair here could never grow.
		prompt := &agent.PermissionPrompt{
			Request: agent.PermissionRequest{
				ToolKind:    toolName,
				ToolTitle:   permissionTitle(input),
				PolicyOwned: true,
			},
			Decision: decision,
		}
		if !sendEvent(sub, agent.Event{Type: agent.EventPermission, Permission: prompt}) {
			return false, "The turn ended before approval could be requested, so this call was denied."
		}
		select {
		case optionID := <-decision:
			switch optionID {
			case agent.PermissionAllow:
				return true, ""
			case agent.PermissionDeny:
				return false, "The user denied this tool call. Do not retry it — ask them how they would like to proceed."
			default:
				// The prompt resolved without a choice: dismissed, or timed out.
				// Worth distinguishing from a deliberate no, because the right
				// next move differs — nobody has actually refused anything.
				return false, "The approval request was dismissed without an answer, so this call was denied."
			}
		case <-done:
			return false, "The session ended before the user answered the approval request, so this call was denied."
		}
	}
}

func (s *procSession) writeControlResponseError(reqID, message string) {
	s.writeJSON(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "error",
			"request_id": reqID,
			"error":      message,
		},
	})
}

// --- stdin writing --------------------------------------------------------

func (s *procSession) writeUser(text string) error {
	return s.writeJSON(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": text},
	})
}

func (s *procSession) writeJSON(v any) error {
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("claudecode: encode frame: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.stdin == nil {
		return errors.New("claudecode: stdin unavailable")
	}
	if _, err := s.stdin.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("claudecode: write frame: %w", err)
	}
	return nil
}

func (s *procSession) stderrSuffix() string {
	s.mu.Lock()
	sb := s.stderr
	s.mu.Unlock()
	if sb == nil {
		return ""
	}
	str := strings.TrimSpace(sb.String())
	if str == "" {
		return ""
	}
	return " [stderr: " + str + "]"
}

// --- shared helpers -------------------------------------------------------

// permissionTitle renders a can_use_tool input into a concise human title — the
// command for a shell call, the path for a file op, else the compact JSON.
func permissionTitle(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(input, &m) == nil {
		for _, k := range []string{"command", "file_path", "path", "pattern", "url"} {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				return v
			}
		}
	}
	return string(input)
}

// sendEvent delivers on a turn's channel, recovering from the race where the turn
// ended and closed it (a permission ask can outlive its turn on a dying process).
func sendEvent(sub *subscription, ev agent.Event) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	sub.events <- ev
	return true
}

// cappedBuffer is a bounded, concurrency-safe io.Writer used to capture a
// process's stderr for diagnostics without growing without limit.
type cappedBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.limit - len(b.buf); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		b.buf = append(b.buf, p[:room]...)
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
