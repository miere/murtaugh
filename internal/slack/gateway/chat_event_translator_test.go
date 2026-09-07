package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

// newTestEventTranslator wires a translator to a recording renderer with a
// liveness window long enough that it never fires by accident. The timing tests
// pass their own window.
func newTestEventTranslator() (*eventTranslator, *recordingRenderer) {
	r := &recordingRenderer{}
	return newEventTranslator(r, time.Hour, discardLogger()), r
}

// drive feeds events through the translator and returns the terminal step,
// failing the test if a delivery error or a second terminal shows up. It mirrors
// what a caller's loop does, minus the policy.
func drive(t *testing.T, tr *eventTranslator, events ...agent.Event) turnStep {
	t.Helper()
	ctx := context.Background()
	for _, ev := range events {
		step, err := tr.Event(ctx, ev)
		if err != nil {
			t.Fatalf("Event(%s): unexpected delivery error: %v", ev.Type, err)
		}
		if step.Kind.terminal() {
			return step
		}
	}
	return tr.Closed()
}

// TestEventTranslatorStatusKeepsTurnAliveWithoutRendering is the demonstration
// #188 asks for: the outbound layer cannot observe anything the inbound layer has
// not handed it.
//
// A status heartbeat is a real event that is deliberately NEVER rendered. Here a
// stream of nothing but heartbeats runs for well over the liveness window and the
// turn stays alive — while the renderer is driven exactly once, by the terminal.
// A detector living on the write path would have seen no calls at all for that
// whole stretch and declared the turn dead; the one at the event edge saw eight
// frames. That is the concrete failure the layering makes structurally
// unavailable (#170 Concern 4), and the mechanical half of the same rule is the
// renderclock analyzer, which forbids a chatRenderer from importing a clock at
// all.
func TestEventTranslatorStatusKeepsTurnAliveWithoutRendering(t *testing.T) {
	r := &recordingRenderer{}
	// 8 heartbeats 10ms apart (~80ms) under a 60ms window: no single gap
	// approaches the window, so it must never elapse — the same shape as
	// TestChatHandlerIdleTimerResetsOnActivity.
	tr := newEventTranslator(r, 60*time.Millisecond, discardLogger())
	defer tr.StopLiveness()
	ctx := context.Background()

	for range 8 {
		time.Sleep(10 * time.Millisecond)
		step, err := tr.Event(ctx, agent.Event{Type: agent.EventStatus, Text: "running a tool"})
		if err != nil {
			t.Fatalf("status event: %v", err)
		}
		if step.Kind != stepContinue {
			t.Fatalf("a heartbeat must not end the turn, got %s", step.Kind)
		}
	}
	select {
	case <-tr.Idle():
		t.Fatal("liveness window elapsed during a steady heartbeat stream: the timer is not being reset on every event")
	default:
	}

	step := drive(t, tr, agent.Event{Type: agent.EventComplete})
	if err := tr.Settle(ctx, step); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := strings.Join(r.calls, ","); got != "Finish" {
		t.Fatalf("the renderer must see nothing but the terminal for a heartbeat-only turn, got calls: [%s]", got)
	}
}

// TestEventTranslatorTextTurnRendersInOrderAndFinishesClean covers the ordinary
// turn: text goes to the renderer as it arrives, the terminal is a plain Finish,
// and no empty-reply note is raised because the turn plainly produced a reply.
func TestEventTranslatorTextTurnRendersInOrderAndFinishesClean(t *testing.T) {
	tr, r := newTestEventTranslator()
	step := drive(t, tr,
		agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "t1", Title: "read"}},
		agent.Event{Type: agent.EventText, Text: "here is "},
		agent.Event{Type: agent.EventText, Text: "what I found"},
		agent.Event{Type: agent.EventComplete, StopReason: "end_turn"},
	)
	if step.Kind != stepFinished {
		t.Fatalf("expected a finished turn, got %s", step.Kind)
	}
	if err := tr.Settle(context.Background(), step); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := strings.Join(r.calls, ","); got != "Task,Text,Text,Finish" {
		t.Fatalf("renderer driven out of stream order: [%s]", got)
	}
	if r.empty != nil {
		t.Errorf("a turn with a reply must not raise the empty-reply note, got %+v", r.empty)
	}
	stats := tr.Stats()
	if stats.Reply != "here is what I found" || stats.Chunks != 2 || stats.Bytes != 20 {
		t.Errorf("transcript accumulation wrong: %+v", stats)
	}
	if stats.Tools != 1 || stats.StopReason != "end_turn" {
		t.Errorf("turn facts wrong: %+v", stats)
	}
}

// TestEventTranslatorAttachmentOnlyTurnIsNotEmpty guards the case that looks like
// a failure and is not: the agent answered with a file and no prose. The
// empty-reply note must be suppressed, or an attachment-only reply reads as a
// turn that did nothing.
func TestEventTranslatorAttachmentOnlyTurnIsNotEmpty(t *testing.T) {
	tr, r := newTestEventTranslator()
	step := drive(t, tr,
		agent.Event{Type: agent.EventAttachment, Attachment: &agent.AttachmentEvent{Filename: "report.md"}},
		agent.Event{Type: agent.EventComplete},
	)
	if err := tr.Settle(context.Background(), step); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := strings.Join(r.calls, ","); got != "Attachment,Finish" {
		t.Fatalf("unexpected renderer calls: [%s]", got)
	}
	if r.empty != nil {
		t.Fatalf("a delivered file is a reply; the empty-reply note must be suppressed, got %+v", r.empty)
	}
	if got := tr.Stats().Attachments; got != 1 {
		t.Errorf("expected 1 delivered attachment, got %d", got)
	}
}

// TestEventTranslatorSilentTurnSaysWhatItDid covers the genuinely empty turn: no
// text, no file, only tool calls. The note must state what the turn ran, and the
// plan entry must not be counted as work — it is the agent's to-do list, not
// something it executed.
func TestEventTranslatorSilentTurnSaysWhatItDid(t *testing.T) {
	tr, r := newTestEventTranslator()
	step := drive(t, tr,
		agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "p1", Kind: agent.TaskKindPlan, Title: "plan"}},
		agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "t1", Title: "read"}},
		// The same tool ticking again is one tool, not two.
		agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "t1", Title: "read", Status: agent.TaskStatusComplete}},
		agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "t2", Title: "write"}},
		agent.Event{Type: agent.EventComplete},
	)
	if err := tr.Settle(context.Background(), step); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if r.empty == nil {
		t.Fatal("a turn with no text and no file must raise the empty-reply note")
	}
	if got := r.empty.Subtitle; !strings.Contains(got, "2 tools") {
		t.Errorf("the note must count tool calls and exclude plan entries, got %q", got)
	}
	if got := tr.Stats().Tools; got != 2 {
		t.Errorf("expected 2 distinct tools, got %d", got)
	}
}

// TestEventTranslatorClosedStreamFinishesLikeComplete: a backend that simply
// stops talking still gets its reply sealed. Losing this is a message left open
// forever, which is the failure the whole liveness concern exists around.
func TestEventTranslatorClosedStreamFinishesLikeComplete(t *testing.T) {
	tr, r := newTestEventTranslator()
	step := drive(t, tr, agent.Event{Type: agent.EventText, Text: "half an answer"})
	if step.Kind != stepFinished {
		t.Fatalf("a closed stream is a finish, got %s", step.Kind)
	}
	if err := tr.Settle(context.Background(), step); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if !r.finished || r.failed != nil {
		t.Fatalf("a closed stream must finish, not fail: finished=%v failed=%v", r.finished, r.failed)
	}
}

// TestEventTranslatorCancellationIsAnInterruptNotAFailure is the identity check
// #187 warns about: a cancellation that renders as a failure card tells the user
// their turn broke when they stopped it themselves. Both shapes must map to the
// interrupt marker — the agent's own aborted-turn error, and a context whose
// cause is a cancellation (the interrupt closure cancels only after a grace
// period, so either can win the race).
func TestEventTranslatorCancellationIsAnInterruptNotAFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ctx   func() (context.Context, context.CancelFunc)
		event agent.Event
	}{
		{
			name:  "agent reports the cancellation",
			ctx:   func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			event: agent.Event{Type: agent.EventError, Error: fmt.Errorf("turn aborted: %w", context.Canceled)},
		},
		{
			name: "our own context carries it first",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			event: agent.Event{Type: agent.EventError, Error: errors.New("stream closed by peer")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, r := newTestEventTranslator()
			ctx, cancel := tc.ctx()
			defer cancel()

			step, err := tr.Event(ctx, tc.event)
			if err != nil {
				t.Fatalf("Event: %v", err)
			}
			if step.Kind != stepInterrupted {
				t.Fatalf("expected an interrupt, got %s", step.Kind)
			}
			if err := tr.Settle(ctx, step); err != nil {
				t.Fatalf("Settle: %v", err)
			}
			if !r.interrupted {
				t.Error("the interrupt marker was not rendered")
			}
			if r.failed != nil {
				t.Errorf("an interrupt must never paint a failure card, got %v", r.failed)
			}
		})
	}
}

// TestEventTranslatorFailurePreservesErrorIdentity is the other half of the same
// concern. The error the caller is handed, and the one the renderer is handed,
// must be the producer's own value: the credential-repair path matches its prose,
// the alert card classifies it, and the tool ceiling is matched by sentinel. Any
// of those breaks the moment the error is re-wrapped or re-worded here.
func TestEventTranslatorFailurePreservesErrorIdentity(t *testing.T) {
	tr, r := newTestEventTranslator()
	wrapped := fmt.Errorf("tool bash exceeded its ceiling: %w", agent.ErrToolCeiling)

	step, err := tr.Event(context.Background(), agent.Event{Type: agent.EventError, Error: wrapped})
	if err != nil {
		t.Fatalf("Event: %v", err)
	}
	if step.Kind != stepFailed {
		t.Fatalf("expected a failure, got %s", step.Kind)
	}
	if step.Err != wrapped {
		t.Fatalf("the step must carry the producer's own error value, got %#v", step.Err)
	}
	if !step.ToolCeiling {
		t.Error("a wrapped ErrToolCeiling must be reported so the caller can drop the wedged session")
	}
	if err := tr.Settle(context.Background(), step); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if r.failed != wrapped {
		t.Fatalf("the renderer must be handed the same error value, got %#v", r.failed)
	}
}

// TestEventTranslatorCallerMaySubstituteTheRenderedError proves the seam the
// two-call shape (Event then Settle) exists for: the credential-repair path
// replaces the backend's "run /login" prose — advice the user cannot act on —
// with something addressed to them, and it does that between noticing the failure
// and rendering it.
func TestEventTranslatorCallerMaySubstituteTheRenderedError(t *testing.T) {
	tr, r := newTestEventTranslator()
	step, err := tr.Event(context.Background(), agent.Event{
		Type:  agent.EventError,
		Error: errors.New("Invalid API key · Please run /login"),
	})
	if err != nil {
		t.Fatalf("Event: %v", err)
	}
	step.Err = errCredentialBlocked
	if err := tr.Settle(context.Background(), step); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if !errors.Is(r.failed, errCredentialBlocked) {
		t.Fatalf("the caller's substituted error must reach the renderer, got %v", r.failed)
	}
}

// TestEventTranslatorStallSealsWithoutPaintingFailure: the liveness window
// elapsing is not a failure. Nothing broke — the agent went quiet and we stopped
// waiting — so the open sections are sealed and no error card is posted. The
// caller's own policy (cancel the session, drop the binding, post the aside) runs
// off the returned step, not in here.
func TestEventTranslatorStallSealsWithoutPaintingFailure(t *testing.T) {
	r := &recordingRenderer{}
	tr := newEventTranslator(r, 20*time.Millisecond, discardLogger())
	defer tr.StopLiveness()

	select {
	case <-tr.Idle():
	case <-time.After(2 * time.Second):
		t.Fatal("the liveness window never elapsed on a silent stream")
	}
	step := tr.Stalled()
	if step.Kind != stepStalled {
		t.Fatalf("expected a stall, got %s", step.Kind)
	}
	if err := tr.Settle(context.Background(), step); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := strings.Join(r.calls, ","); got != "EnsureStopped" {
		t.Fatalf("a stall seals and stops there, got calls: [%s]", got)
	}
}

// TestEventTranslatorPermissionSettlesTheReplyFirst: an approval card posted
// while the reply is still streaming makes the reply look truncated. The
// translator settles the open text before handing the prompt back — ordering is a
// rendering decision — but it never answers it: who may approve is gateway
// policy, and a permission is not a terminal.
func TestEventTranslatorPermissionSettlesTheReplyFirst(t *testing.T) {
	tr, r := newTestEventTranslator()
	prompt := &agent.PermissionPrompt{
		Request:  agent.PermissionRequest{ToolKind: "execute"},
		Decision: make(chan string, 1),
	}
	step, err := tr.Event(context.Background(), agent.Event{Type: agent.EventPermission, Permission: prompt})
	if err != nil {
		t.Fatalf("Event: %v", err)
	}
	if step.Kind != stepPermission || step.Kind.terminal() {
		t.Fatalf("a permission request is a non-terminal interjection, got %s", step.Kind)
	}
	if step.Permission != prompt {
		t.Fatal("the prompt must be handed back unchanged so the caller can answer its channel")
	}
	if got := strings.Join(r.calls, ","); got != "BeginInterjection" {
		t.Fatalf("expected the open reply to be settled and nothing else, got calls: [%s]", got)
	}
	if len(prompt.Decision) != 0 {
		t.Fatal("the translator must not answer a permission request itself")
	}
}

// TestEventTranslatorDeliveryFailureIsNotATerminal separates the two errors that
// look alike: Slack refusing a write is the caller's to return (its deferred
// EnsureStopped seals whatever is open), while a failure the agent reported comes
// back as a terminal step. Conflating them would have a Slack outage render as
// "the agent failed".
func TestEventTranslatorDeliveryFailureIsNotATerminal(t *testing.T) {
	r := &recordingRenderer{textErr: errors.New("channel_type_not_supported")}
	tr := newEventTranslator(r, time.Hour, discardLogger())

	step, err := tr.Event(context.Background(), agent.Event{Type: agent.EventText, Text: "hello"})
	if err == nil {
		t.Fatal("expected the renderer's delivery failure to be reported")
	}
	if step.Kind.terminal() {
		t.Fatalf("a delivery failure is not a terminal step, got %s", step.Kind)
	}
	if r.failed != nil {
		t.Fatalf("a delivery failure must not be rendered as an agent failure, got %v", r.failed)
	}
}

// TestEventTranslatorIgnoresAnUnknownEventKind: a node on a newer build must not
// be able to abort a live conversation by emitting a kind this gateway has not
// learnt yet. The wire codec is where an unknown kind IS an error, because there
// it is still recoverable.
func TestEventTranslatorIgnoresAnUnknownEventKind(t *testing.T) {
	tr, r := newTestEventTranslator()
	step, err := tr.Event(context.Background(), agent.Event{Type: agent.EventType("telemetry")})
	if err != nil {
		t.Fatalf("an unknown kind must not error: %v", err)
	}
	if step.Kind != stepContinue {
		t.Fatalf("an unknown kind must not end the turn, got %s", step.Kind)
	}
	if len(r.calls) != 0 {
		t.Fatalf("an unknown kind must render nothing, got calls: %v", r.calls)
	}
}

// TestEventTranslatorSettleRejectsANonTerminal guards the one misuse the shape
// allows: settling on a step that has not ended the turn would close the reply
// mid-stream and leave the rest of it homeless.
func TestEventTranslatorSettleRejectsANonTerminal(t *testing.T) {
	tr, r := newTestEventTranslator()
	if err := tr.Settle(context.Background(), turnStep{Kind: stepContinue}); err == nil {
		t.Fatal("expected settling a non-terminal step to be refused")
	}
	if len(r.calls) != 0 {
		t.Fatalf("nothing may be rendered for a refused settle, got calls: %v", r.calls)
	}
}
