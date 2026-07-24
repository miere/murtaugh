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
//
// `--permission-prompt-tool stdio` is the enabler for the control-protocol
// permission route: it is a reserved sentinel (not an MCP tool name) that tells
// the CLI to ask the controlling process for tool permission via a can_use_tool
// control_request instead of auto-denying. Verified against 2.1.216 — without it
// a headless turn silently denies every gated tool (spec 019 §6).
var defaultArgs = []string{
	"-p",
	"--input-format", "stream-json",
	"--output-format", "stream-json",
	"--verbose",
	"--permission-prompt-tool", "stdio",
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
	if err := sess.startSession(ctx); err != nil {
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
	return nil
}

// --- procSession: one process == one session ------------------------------

type subscription struct {
	events chan agent.Event
}

type procSession struct {
	id     string
	log    *slog.Logger
	opts   Options // shared launch config + policy + OnBackground
	policy string

	mu       sync.Mutex
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stderr   *cappedBuffer
	procDone chan struct{}
	closed   bool

	active  *subscription
	pending map[string]chan *streamMessage
	reqSeq  atomic.Int64
}

func newProcSession(id string, opts Options) *procSession {
	return &procSession{
		id:      id,
		log:     opts.Logger.With("session", id),
		opts:    opts,
		policy:  strings.ToLower(strings.TrimSpace(opts.PermissionPolicy)),
		pending: make(map[string]chan *streamMessage),
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
	args := append(append([]string{}, s.opts.Args...), sessionArgs...)
	cmd := exec.Command(s.opts.Command, args...)
	cmd.Dir = s.opts.WorkDir
	if len(s.opts.Env) > 0 {
		cmd.Env = append(cmd.Environ(), s.opts.Env...)
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
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func (s *procSession) prompt(req agent.PromptRequest) (<-chan agent.Event, error) {
	sub := &subscription{events: make(chan agent.Event, 64)}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("claudecode: session is closed")
	}
	s.active = sub
	s.mu.Unlock()

	if err := s.writeUser(req.Text); err != nil {
		s.mu.Lock()
		s.active = nil
		s.mu.Unlock()
		close(sub.events)
		return nil, err
	}
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
	s.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
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

func (s *procSession) answerPermission(reqID string, msg *streamMessage) {
	toolName, input := parseCanUseTool(msg.Request)
	allow := s.decidePermission(toolName, input)
	behavior := "deny"
	if allow {
		behavior = "allow"
	}
	inner := map[string]any{"behavior": behavior}
	if allow && len(input) > 0 {
		inner["updatedInput"] = json.RawMessage(input)
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
func (s *procSession) decidePermission(toolName string, input json.RawMessage) bool {
	switch s.policy {
	case "auto-allow":
		return true
	case "auto-deny":
		return false
	default: // ask
		s.mu.Lock()
		sub := s.active
		done := s.procDone
		s.mu.Unlock()
		if sub == nil {
			s.log.Warn("claudecode: permission request with no active turn; denying", "tool", toolName)
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
			return false
		}
		select {
		case optionID := <-decision:
			return optionID == "allow"
		case <-done:
			return false
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
