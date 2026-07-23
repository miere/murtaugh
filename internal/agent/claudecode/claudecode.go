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

	"github.com/miere/murtaugh/internal/agent"
)

// defaultArgs launches `claude` in headless bidirectional stream-json mode: a
// long-lived process that reads NDJSON user turns on stdin and streams NDJSON
// events on stdout. --verbose is required with --output-format=stream-json.
var defaultArgs = []string{
	"-p",
	"--input-format", "stream-json",
	"--output-format", "stream-json",
	"--verbose",
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
	// OnUnsolicited receives events that arrive with no active turn — chiefly a
	// background subagent's post-`result` completion. Phase 1 logs them; Phase 2
	// routes them back into the originating Slack thread (spec 019 §5). nil drops.
	OnUnsolicited func(agent.Event)
}

// Client is a single long-lived Claude Code stream-json session, implementing
// agent.Client. One Client == one `claude` process == one conversation, held open
// across turns (so a background subagent completing after a turn's `result` still
// reaches us). SessionManager caches one per ConversationKey.
//
// The process is started lazily in NewSession (not Initialize): only there is the
// Slack conversation metadata known, and the deterministic --session-id is derived
// from it. Start prefers --resume (an existing on-disk session) and falls back to
// --session-id (a fresh one) — restart resilience with no persisted mapping.
type Client struct {
	opts Options
	log  *slog.Logger

	mu       sync.Mutex
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stderr   *cappedBuffer
	procDone chan struct{} // closed when the current process's read loop ends
	closed   bool
	session  string

	// active is the subscription for the in-flight turn; nil between turns.
	active *subscription

	// pending correlates a control_request we sent (by request_id) with its
	// control_response.
	pending map[string]chan *streamMessage
	reqSeq  atomic.Int64
}

type subscription struct {
	events chan agent.Event
}

// New builds a Client. It does not start the process; call Initialize then
// NewSession (which starts it).
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
	return &Client{opts: opts, log: opts.Logger, pending: make(map[string]chan *streamMessage)}
}

// Initialize validates the client is ready. The process is started lazily in
// NewSession, where the conversation metadata (and thus the --session-id) is known.
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
// starts the process bound to it (resume-preferred, create-fallback). The returned
// id is authoritative — it is the one we passed via --session-id/--resume.
func (c *Client) NewSession(ctx context.Context, meta agent.SessionMetadata) (agent.Session, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return agent.Session{}, errors.New("claudecode: client is closed")
	}
	sessionID := deriveSessionID(meta)
	if err := c.startSession(ctx, sessionID); err != nil {
		return agent.Session{}, err
	}
	c.mu.Lock()
	c.session = sessionID
	c.mu.Unlock()
	return agent.Session{ID: sessionID}, nil
}

// startSession brings up the process for sessionID, preferring to resume an
// existing on-disk session and falling back to creating a fresh one. `claude`
// makes --session-id create-only ("already in use" on an existing id), so resume
// must be a distinct attempt.
func (c *Client) startSession(ctx context.Context, sessionID string) error {
	if err := c.spawnAndHandshake(ctx, []string{"--resume", sessionID}); err == nil {
		return nil
	} else {
		c.log.Debug("claudecode: resume failed; creating fresh session", "session", sessionID, "error", err)
		resumeErr := err
		c.teardownProc()
		if createErr := c.spawnAndHandshake(ctx, []string{"--session-id", sessionID}); createErr != nil {
			return fmt.Errorf("claudecode: start session %s: resume=%v; create=%w", sessionID, resumeErr, createErr)
		}
		return nil
	}
}

func (c *Client) spawnAndHandshake(ctx context.Context, sessionArgs []string) error {
	if err := c.start(sessionArgs); err != nil {
		return err
	}
	if err := c.handshake(ctx); err != nil {
		return fmt.Errorf("%w%s", err, c.stderrSuffix())
	}
	return nil
}

// start spawns the process with the base args plus session-selection args, wires
// stdio, and launches the read loop. It replaces any prior process handle.
func (c *Client) start(sessionArgs []string) error {
	args := append(append([]string{}, c.opts.Args...), sessionArgs...)
	cmd := exec.Command(c.opts.Command, args...)
	cmd.Dir = c.opts.WorkDir
	if len(c.opts.Env) > 0 {
		cmd.Env = append(cmd.Environ(), c.opts.Env...)
	}
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
		return fmt.Errorf("claudecode: start %q: %w", c.opts.Command, err)
	}
	done := make(chan struct{})
	c.mu.Lock()
	c.cmd = cmd
	c.stdin = stdin
	c.stderr = stderr
	c.procDone = done
	c.mu.Unlock()
	go func() {
		c.readLoop(stdout)
		_ = cmd.Wait()
		close(done)
	}()
	return nil
}

// handshake sends the initialize control_request and waits for its response —
// the CLI requires it before honouring any other control request (permissions,
// interrupt). It fails fast if the process exits first (e.g. a failed --resume).
func (c *Client) handshake(ctx context.Context) error {
	resp, err := c.sendControl(ctx, map[string]any{"subtype": "initialize"})
	if err != nil {
		return fmt.Errorf("claudecode: initialize handshake: %w", err)
	}
	if controlResponseSubtype(resp) == "error" {
		return fmt.Errorf("claudecode: initialize rejected: %s", controlResponseError(resp))
	}
	return nil
}

// teardownProc kills the current process without closing the Client, so a failed
// resume attempt can be replaced by a create attempt.
func (c *Client) teardownProc() {
	c.mu.Lock()
	stdin, cmd := c.stdin, c.cmd
	c.stdin, c.cmd = nil, nil
	c.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// Prompt sends one user turn and returns the event stream for it. The channel
// closes when the turn's `result` arrives; the process stays alive for the next
// turn and for any background completion.
func (c *Client) Prompt(_ context.Context, _ string, req agent.PromptRequest) (<-chan agent.Event, error) {
	sub := &subscription{events: make(chan agent.Event, 64)}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("claudecode: client is closed")
	}
	c.active = sub
	c.mu.Unlock()

	if err := c.writeUser(req.Text); err != nil {
		c.mu.Lock()
		c.active = nil
		c.mu.Unlock()
		close(sub.events)
		return nil, err
	}
	return sub.events, nil
}

// Cancel interrupts the in-flight turn via the control channel. The session
// survives; any background subagents keep running (stop those with stop_task).
func (c *Client) Cancel(ctx context.Context, _ string) error {
	_, err := c.sendControl(ctx, map[string]any{"subtype": "interrupt"})
	return err
}

// Close terminates the process. It is safe to call more than once.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	return nil
}

// --- stream reading -------------------------------------------------------

func (c *Client) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		msg, err := decodeMessage(scanner.Bytes())
		if err != nil {
			c.log.Warn("claudecode: undecodable stream line", "error", err)
			continue
		}
		if msg == nil {
			continue
		}
		c.dispatch(msg)
	}
	c.failActive(errors.New("claudecode: stream closed"))
}

func (c *Client) dispatch(msg *streamMessage) {
	switch {
	case msg.Type == "control_response":
		c.deliverControlResponse(msg)
	case msg.Type == "control_request":
		go c.handleControlRequest(msg)
	case msg.isResult():
		stop := msg.StopReason
		if stop == "" {
			stop = "end_turn"
		}
		c.completeActive(stop)
	default:
		for _, ev := range msg.toEvents() {
			c.emit(ev)
		}
	}
}

// emit delivers an event to the active turn, or to OnUnsolicited when a turn is
// not open (a background completion arriving after `result`).
func (c *Client) emit(ev agent.Event) {
	c.mu.Lock()
	sub := c.active
	c.mu.Unlock()
	if sub != nil {
		sub.events <- ev
		return
	}
	if c.opts.OnUnsolicited != nil {
		c.opts.OnUnsolicited(ev)
		return
	}
	c.log.Debug("claudecode: dropping unsolicited event (no active turn)", "type", string(ev.Type))
}

func (c *Client) completeActive(stopReason string) {
	c.mu.Lock()
	sub := c.active
	c.active = nil
	c.mu.Unlock()
	if sub == nil {
		return
	}
	sub.events <- agent.Event{Type: agent.EventComplete, StopReason: stopReason}
	close(sub.events)
}

func (c *Client) failActive(err error) {
	c.mu.Lock()
	sub := c.active
	c.active = nil
	c.mu.Unlock()
	if sub == nil {
		return
	}
	sub.events <- agent.Event{Type: agent.EventError, Error: err}
	close(sub.events)
}

// --- control protocol -----------------------------------------------------

// sendControl writes a control_request and waits for the matching control_response,
// unblocking early if the process exits or ctx is cancelled.
func (c *Client) sendControl(ctx context.Context, request map[string]any) (*streamMessage, error) {
	id := fmt.Sprintf("req-%d", c.reqSeq.Add(1))
	ch := make(chan *streamMessage, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("claudecode: client is closed")
	}
	done := c.procDone
	c.pending[id] = ch
	c.mu.Unlock()
	if done == nil {
		done = make(chan struct{}) // never closes: no process yet, rely on ctx
	}

	frame := map[string]any{"type": "control_request", "request_id": id, "request": request}
	if err := c.writeJSON(frame); err != nil {
		c.dropPending(id)
		return nil, err
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-done:
		c.dropPending(id)
		return nil, errors.New("claudecode: process exited before control response")
	case <-ctx.Done():
		c.dropPending(id)
		return nil, ctx.Err()
	}
}

func (c *Client) dropPending(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Client) deliverControlResponse(msg *streamMessage) {
	id := controlResponseRequestID(msg)
	if id == "" {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch != nil {
		ch <- msg
	}
}

// handleControlRequest serves a control_request the CLI sends us. The only one we
// implement is can_use_tool (a tool-permission decision); anything else is
// acknowledged with an error so the CLI fails fast rather than hanging.
func (c *Client) handleControlRequest(msg *streamMessage) {
	sub := controlRequestSubtype(msg)
	reqID := rawString(msg.RequestID)
	switch sub {
	case "can_use_tool":
		c.answerPermission(reqID, msg)
	default:
		c.writeControlResponseError(reqID, fmt.Sprintf("claudecode: unsupported control request %q", sub))
	}
}

func (c *Client) answerPermission(reqID string, msg *streamMessage) {
	toolName, input := parseCanUseTool(msg.Request)
	allow := c.decidePermission(toolName, input)
	behavior := "deny"
	if allow {
		behavior = "allow"
	}
	inner := map[string]any{"behavior": behavior}
	if allow && len(input) > 0 {
		inner["updatedInput"] = json.RawMessage(input)
	}
	c.writeJSON(map[string]any{
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
// blocks on the human's decision. No active turn (headless) or a dead process
// denies, so a turn never hangs on an answer that cannot come.
func (c *Client) decidePermission(toolName string, input json.RawMessage) bool {
	switch strings.ToLower(strings.TrimSpace(c.opts.PermissionPolicy)) {
	case "auto-allow":
		return true
	case "auto-deny":
		return false
	default: // ask
		c.mu.Lock()
		sub := c.active
		done := c.procDone
		c.mu.Unlock()
		if sub == nil {
			c.log.Warn("claudecode: permission request with no active turn; denying", "tool", toolName)
			return false
		}
		decision := make(chan string, 1)
		prompt := &agent.PermissionPrompt{
			Request: agent.PermissionRequest{
				ToolKind:  toolName,
				ToolTitle: permissionTitle(input),
				Options: []agent.PermissionOption{
					{ID: "allow", Name: "Allow", Kind: "allow_once"},
					{ID: "deny", Name: "Deny", Kind: "reject_once"},
				},
			},
			Decision: decision,
		}
		if !sendEvent(sub, agent.Event{Type: agent.EventPermission, Permission: prompt}) {
			return false // turn ended underneath us
		}
		select {
		case optionID := <-decision:
			return optionID == "allow"
		case <-done:
			return false
		}
	}
}

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

func (c *Client) writeControlResponseError(reqID, message string) {
	c.writeJSON(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "error",
			"request_id": reqID,
			"error":      message,
		},
	})
}

// --- stdin writing --------------------------------------------------------

func (c *Client) writeUser(text string) error {
	return c.writeJSON(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": text},
	})
}

func (c *Client) writeJSON(v any) error {
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("claudecode: encode frame: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.stdin == nil {
		return errors.New("claudecode: stdin unavailable")
	}
	if _, err := c.stdin.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("claudecode: write frame: %w", err)
	}
	return nil
}

func (c *Client) stderrSuffix() string {
	c.mu.Lock()
	sb := c.stderr
	c.mu.Unlock()
	if sb == nil {
		return ""
	}
	s := strings.TrimSpace(sb.String())
	if s == "" {
		return ""
	}
	return " [stderr: " + s + "]"
}

// cappedBuffer is a bounded, concurrency-safe io.Writer used to capture a
// process's stderr for diagnostics without growing without limit over a
// long-lived session.
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
