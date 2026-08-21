package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

// helperArgs builds the args that re-invoke this test binary as a fake `claude`
// stream-json process in the given mode.
func helperArgs(mode string) []string {
	return []string{"-test.run", "TestClaudeHelperProcess", "--", mode}
}

func newHelperClient(t *testing.T, mode string, opts Options) *Client {
	t.Helper()
	opts.Command = os.Args[0]
	opts.Args = helperArgs(mode)
	c := New(opts)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func drain(t *testing.T, ch <-chan agent.Event, timeout time.Duration) []agent.Event {
	t.Helper()
	var got []agent.Event
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timed out draining events; got %d so far: %+v", len(got), got)
		}
	}
}

func TestInitializeHandshakeAndBasicTurn(t *testing.T) {
	c := newHelperClient(t, "basic", Options{})
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sess, err := c.NewSession(ctx, agent.SessionMetadata{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if want := agent.DeriveSessionID(agent.SessionMetadata{}); sess.ID != want {
		t.Fatalf("expected derived session id %q, got %q", want, sess.ID)
	}
	ch, err := c.Prompt(ctx, sess.ID, agent.PromptRequest{Text: "hi"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	got := drain(t, ch, 5*time.Second)
	if len(got) == 0 || got[0].Type != agent.EventText || got[0].Text != "hello from fake" {
		t.Fatalf("expected leading text event, got %+v", got)
	}
	last := got[len(got)-1]
	if last.Type != agent.EventComplete || last.StopReason != "end_turn" {
		t.Fatalf("expected trailing EventComplete end_turn, got %+v", last)
	}
}

// TestConcurrentSessionsAreIndependent proves the multiplex: one Client serving
// two conversations (as the gateway does per agent) runs a separate process per
// session, so they don't clobber each other.
func TestConcurrentSessionsAreIndependent(t *testing.T) {
	c := newHelperClient(t, "basic", Options{})
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	a, err := c.NewSession(ctx, agent.SessionMetadata{TeamID: "T", ChannelID: "C", ThreadTS: "1"})
	if err != nil {
		t.Fatalf("NewSession a: %v", err)
	}
	b, err := c.NewSession(ctx, agent.SessionMetadata{TeamID: "T", ChannelID: "C", ThreadTS: "2"})
	if err != nil {
		t.Fatalf("NewSession b: %v", err)
	}
	if a.ID == b.ID {
		t.Fatalf("distinct threads collided on session id %q", a.ID)
	}
	for _, id := range []string{a.ID, b.ID} {
		ch, err := c.Prompt(ctx, id, agent.PromptRequest{Text: "hi"})
		if err != nil {
			t.Fatalf("Prompt %s: %v", id, err)
		}
		got := drain(t, ch, 5*time.Second)
		if len(got) == 0 || got[0].Type != agent.EventText || got[0].Text != "hello from fake" {
			t.Fatalf("session %s: unexpected events %+v", id, got)
		}
	}
}

// TestPermissionAskRoutesToHuman drives the "ask" policy: the client raises an
// EventPermission (the Slack approval card) and the human's chosen option decides
// allow/deny — the same mechanism the chat handler already serves for ACP.
func TestPermissionAskRoutesToHuman(t *testing.T) {
	cases := []struct {
		name     string
		decision string // the option id a "human" picks
		want     string
	}{
		{"allow", "allow", "permitted:allow|"},
		{"deny", "deny", "permitted:deny|The user denied this tool call. Do not retry it — ask them how they would like to proceed."},
		{"dismiss-denies", "", "permitted:deny|The approval request was dismissed without an answer, so this call was denied."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newHelperClient(t, "perm", Options{}) // default policy = ask
			ctx := context.Background()
			if err := c.Initialize(ctx); err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			sess, err := c.NewSession(ctx, agent.SessionMetadata{})
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			ch, err := c.Prompt(ctx, sess.ID, agent.PromptRequest{Text: "please write a file"})
			if err != nil {
				t.Fatalf("Prompt: %v", err)
			}

			var text string
			var sawPermission bool
			deadline := time.After(5 * time.Second)
			for done := false; !done; {
				select {
				case ev, ok := <-ch:
					if !ok {
						done = true
						break
					}
					switch ev.Type {
					case agent.EventPermission:
						sawPermission = true
						if ev.Permission.Request.ToolKind != "Write" {
							t.Errorf("expected tool kind Write, got %q", ev.Permission.Request.ToolKind)
						}
						ev.Permission.Decision <- tc.decision // stand in for the human
					case agent.EventText:
						text += ev.Text
					case agent.EventComplete:
						done = true
					}
				case <-deadline:
					t.Fatalf("timed out; text so far %q", text)
				}
			}
			if !sawPermission {
				t.Fatal("expected an EventPermission to be raised")
			}
			if text != tc.want {
				t.Fatalf("expected reply %q, got %q", tc.want, text)
			}
		})
	}
}

// TestPermissionAutoPolicies covers the non-interactive policies, which answer
// can_use_tool without raising an EventPermission.
func TestPermissionAutoPolicies(t *testing.T) {
	cases := []struct {
		policy string
		want   string
	}{
		{"auto-allow", "permitted:allow|"},
		{"auto-deny", "permitted:deny|Murtaugh is configured to deny every tool call in this session."},
	}
	for _, tc := range cases {
		t.Run(tc.policy, func(t *testing.T) {
			c := newHelperClient(t, "perm", Options{PermissionPolicy: tc.policy})
			ctx := context.Background()
			if err := c.Initialize(ctx); err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			sess, err := c.NewSession(ctx, agent.SessionMetadata{})
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			ch, err := c.Prompt(ctx, sess.ID, agent.PromptRequest{Text: "write a file"})
			if err != nil {
				t.Fatalf("Prompt: %v", err)
			}
			var text string
			for _, ev := range drain(t, ch, 5*time.Second) {
				if ev.Type == agent.EventPermission {
					t.Fatal("auto policy must not raise EventPermission")
				}
				if ev.Type == agent.EventText {
					text += ev.Text
				}
			}
			if text != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, text)
			}
		})
	}
}

func TestCancelInterruptsTurn(t *testing.T) {
	c := newHelperClient(t, "hang", Options{})
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sess, err := c.NewSession(ctx, agent.SessionMetadata{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	ch, err := c.Prompt(ctx, sess.ID, agent.PromptRequest{Text: "long task"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	// The hang helper never completes on its own; Cancel must interrupt it.
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = c.Cancel(ctx, sess.ID)
	}()
	got := drain(t, ch, 5*time.Second)
	last := got[len(got)-1]
	// A cancelled turn must NOT arrive as a completion. The CLI answers an
	// interrupt with a result that carries no reply text, so completing the turn
	// would hand the relay "the agent finished and said nothing" — and earn the
	// user a warning about an agent that did exactly as it was told.
	if last.Type != agent.EventError {
		t.Fatalf("expected a terminal EventError for an interrupted turn, got %+v", got)
	}
	if !errors.Is(last.Error, context.Canceled) {
		t.Fatalf("interrupt must surface as a cancellation, got %v", last.Error)
	}
}

// TestAbortedTurnWithoutCancelIsAFailure guards the other half of the branch: an
// abnormal end nobody asked for is a real failure and must be reported as one,
// not quietly swallowed as a cancellation (which the relay renders as a benign
// "_interrupted_").
func TestAbortedTurnWithoutCancelIsAFailure(t *testing.T) {
	c := newHelperClient(t, "abort", Options{})
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sess, err := c.NewSession(ctx, agent.SessionMetadata{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	ch, err := c.Prompt(ctx, sess.ID, agent.PromptRequest{Text: "go"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	got := drain(t, ch, 5*time.Second)
	last := got[len(got)-1]
	if last.Type != agent.EventError {
		t.Fatalf("expected a terminal EventError, got %+v", got)
	}
	if errors.Is(last.Error, context.Canceled) {
		t.Fatalf("an uncancelled abort must not masquerade as a cancellation: %v", last.Error)
	}
}

// TestMaxTurnsStillCompletes pins the exclusion: `error_max_turns` carries
// is_error too, but that turn ended on a rule the agent was configured with,
// having very possibly replied first. It must stay a completion.
func TestMaxTurnsStillCompletes(t *testing.T) {
	c := newHelperClient(t, "maxturns", Options{})
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sess, err := c.NewSession(ctx, agent.SessionMetadata{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	ch, err := c.Prompt(ctx, sess.ID, agent.PromptRequest{Text: "go"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	got := drain(t, ch, 5*time.Second)
	last := got[len(got)-1]
	if last.Type != agent.EventComplete || last.StopReason != "end_turn" {
		t.Fatalf("expected a completion, got %+v", got)
	}
}

// --- fake `claude` stream-json process ------------------------------------

// TestClaudeHelperProcess runs as a normal (no-op) test in the parent suite, but
// when re-invoked as a subprocess with a mode arg after "--" it impersonates a
// Claude Code stream-json process: emits system/init, answers the initialize
// handshake, and drives the mode's scenario.
func TestClaudeHelperProcess(t *testing.T) {
	mode := helperMode()
	if mode == "" {
		return // ordinary test run: not the subprocess
	}
	runFakeClaude(mode)
	os.Exit(0)
}

func helperMode() string {
	args := os.Args
	for i, a := range args {
		if a == "--" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func runFakeClaude(mode string) {
	emit := func(v map[string]any) {
		b, _ := json.Marshal(v)
		fmt.Fprintln(os.Stdout, string(b))
	}
	emit(map[string]any{"type": "system", "subtype": "init", "session_id": "sess-1"})

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		var msg map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		switch msg["type"] {
		case "control_request":
			req, _ := msg["request"].(map[string]any)
			sub, _ := req["subtype"].(string)
			reqID := msg["request_id"]
			switch sub {
			case "initialize":
				emit(controlSuccess(reqID, map[string]any{}))
			case "interrupt":
				emit(controlSuccess(reqID, map[string]any{"still_queued": []any{}}))
				emit(abortedResultMsg())
			}
		case "user":
			switch mode {
			case "perm":
				// Ask the client for permission; the reply drives the outcome.
				emit(map[string]any{
					"type":       "control_request",
					"request_id": "srv-1",
					"request": map[string]any{
						"subtype":   "can_use_tool",
						"tool_name": "Write",
						"input":     map[string]any{"file_path": "out.txt"},
					},
				})
			case "hang":
				// Never completes on its own — the test must Cancel.
			case "abort":
				// Ends abnormally without anyone asking it to: a crash mid-turn,
				// not a cancellation.
				emit(abortedResultMsg())
			case "stray":
				// The resume case: the CLI drains a prompt of its own (an
				// orphaned-background-task notification) before ours, and reports
				// it on the same stream, ahead of the answer we asked for.
				emit(map[string]any{
					"type": "system", "subtype": "task_notification",
					"task_id": "bu0b2ajny", "status": "stopped",
					"summary": "No completion record was found for this background shell command from the previous session.",
				})
				emit(strayResultMsg())
				emit(assistantText("hello from fake"))
				emit(resultMsg("end_turn"))
			case "maxturns":
				// Ends on the configured turn limit — an error subtype, but a
				// legitimate end of turn that may well have produced a reply.
				emit(assistantText("as far as I got"))
				emit(map[string]any{"type": "result", "subtype": "error_max_turns", "is_error": true, "stop_reason": "end_turn"})
			default:
				emit(assistantText("hello from fake"))
				emit(resultMsg("end_turn"))
			}
		case "control_response":
			// The client's answer to our can_use_tool ask. Both the behavior and
			// the deny message are echoed: the real CLI rejects a deny that
			// carries no message, so the message is part of the contract, not
			// decoration.
			behavior := "deny"
			var message string
			if resp, ok := msg["response"].(map[string]any); ok {
				if inner, ok := resp["response"].(map[string]any); ok {
					if b, ok := inner["behavior"].(string); ok {
						behavior = b
					}
					message, _ = inner["message"].(string)
				}
			}
			emit(assistantText("permitted:" + behavior + "|" + message))
			emit(resultMsg("end_turn"))
		}
	}
}

func controlSuccess(reqID any, response map[string]any) map[string]any {
	return map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": reqID,
			"response":   response,
		},
	}
}

func assistantText(text string) map[string]any {
	return map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "text", "text": text}},
		},
	}
}

func resultMsg(stop string) map[string]any {
	return map[string]any{"type": "result", "subtype": "success", "stop_reason": stop, "result": ""}
}

// abortedResultMsg is the result the real CLI (verified against 2.1.229) sends
// when a turn stops mid-execution — chiefly the answer to an interrupt. Note the
// stop reason: it is whatever the last assistant message carried, so an
// interrupt mid-tool reports `tool_use` and one between tools reports
// `end_turn`. Only the subtype distinguishes it from a completion, which is why
// the client must read that and not the stop reason.
func abortedResultMsg() map[string]any {
	return map[string]any{
		"type":        "result",
		"subtype":     "error_during_execution",
		"is_error":    true,
		"stop_reason": "tool_use",
		"result":      nil,
	}
}

// strayResultMsg is the result the real CLI (verified against 2.1.238) sends
// after draining a prompt from its own queue — reproduced by resuming a session
// whose previous process died with a background shell still running. It reports
// no turns and no stop reason because it never called the model.
func strayResultMsg() map[string]any {
	return map[string]any{
		"type":      "result",
		"subtype":   "success",
		"is_error":  false,
		"num_turns": 0,
		"result":    "",
	}
}

// TestStrayResultDoesNotCloseOurTurn pins the resume case: the CLI answers a
// prompt of its own before ours, and that answer must not be mistaken for the
// end of the turn we asked for. Getting this wrong hands the gateway a turn with
// no reply text, so the user is told the agent finished without saying anything
// while their actual question is still running.
func TestStrayResultDoesNotCloseOurTurn(t *testing.T) {
	c := newHelperClient(t, "stray", Options{})
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sess, err := c.NewSession(ctx, agent.SessionMetadata{})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	ch, err := c.Prompt(ctx, sess.ID, agent.PromptRequest{Text: "hi"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	got := drain(t, ch, 5*time.Second)
	var completes int
	var text string
	for _, ev := range got {
		switch ev.Type {
		case agent.EventComplete:
			completes++
		case agent.EventText:
			text += ev.Text
		case agent.EventError:
			t.Fatalf("stray result surfaced as an error: %v", ev.Error)
		}
	}
	if completes != 1 {
		t.Fatalf("got %d completions, want exactly 1: %+v", completes, got)
	}
	if text != "hello from fake" {
		t.Fatalf("turn closed before the reply arrived; text = %q, events %+v", text, got)
	}
	if last := got[len(got)-1]; last.Type != agent.EventComplete || last.StopReason != "end_turn" {
		t.Fatalf("expected trailing EventComplete end_turn, got %+v", last)
	}
}

// The claude CLI's own AskUserQuestion renders in the terminal UI, which a
// headless session has not got, so it fails every time it is called. Murtaugh
// publishes a working replacement over MCP under the same name; suppressing the
// built-in is what lets the model find it. Drop this flag and the built-in
// shadows the replacement again.
func TestDefaultArgsSuppressTheBuiltInAskUserQuestion(t *testing.T) {
	var found bool
	for i, arg := range defaultArgs {
		if arg != "--disallowedTools" {
			continue
		}
		if i+1 >= len(defaultArgs) {
			t.Fatal("--disallowedTools has no value")
		}
		if defaultArgs[i+1] != "AskUserQuestion" {
			t.Fatalf("--disallowedTools %q, want AskUserQuestion", defaultArgs[i+1])
		}
		found = true
	}
	if !found {
		t.Fatal("defaultArgs no longer suppresses the built-in AskUserQuestion")
	}
}

// A claude_code session never reads assets/system-prompt.md, so this flag is
// the only channel Murtaugh has for the Slack dialect rules. Without it the
// model falls back to the CLI's own "GitHub-flavored markdown for a terminal"
// instruction and mrkdwn surfaces show raw metacharacters.
func TestDefaultArgsAppendTheSlackFormattingRules(t *testing.T) {
	var value string
	for i, arg := range defaultArgs {
		if arg != "--append-system-prompt" {
			continue
		}
		if i+1 >= len(defaultArgs) {
			t.Fatal("--append-system-prompt has no value")
		}
		value = defaultArgs[i+1]
	}
	if value == "" {
		t.Fatal("defaultArgs no longer appends the Slack formatting rules")
	}
	for _, want := range []string{"standard Markdown", "mrkdwn"} {
		if !strings.Contains(value, want) {
			t.Fatalf("appended prompt does not mention %q:\n%s", want, value)
		}
	}
}
