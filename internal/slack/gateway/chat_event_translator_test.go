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

func newTestEventTranslator() (*eventTranslator, *recordingRenderer) {
	r := &recordingRenderer{}
	return newEventTranslator(r, time.Hour, discardLogger()), r
}

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

// A heartbeat is never rendered, so a liveness check on the write path would call
// this turn dead; the one at the event edge keeps it alive.
func TestEventTranslatorStatusKeepsTurnAliveWithoutRendering(t *testing.T) {
	r := &recordingRenderer{}
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

// A reply that is only a file must not get the empty-reply note, or it reads as a
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

// The plan entry is not counted as work because it is the agent's to-do list, not
// something it ran.
func TestEventTranslatorSilentTurnSaysWhatItDid(t *testing.T) {
	tr, r := newTestEventTranslator()
	step := drive(t, tr,
		agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "p1", Kind: agent.TaskKindPlan, Title: "plan"}},
		agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "t1", Title: "read"}},
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

// A backend that stops talking without a terminal event would otherwise leave its
// message open forever.
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

// A cancellation drawn as a failure tells users their turn broke when they stopped
// it. Either shape can win, since the interrupt cancels only after a grace period.
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

// Credential repair matches the error's text, the alert card classifies it and the
// tool ceiling matches its sentinel, so it must never be re-wrapped.
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

// Event and Settle are separate calls so credential repair can swap the backend's
// "run /login" advice for text the user can act on.
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

// A stall is not a failure: nothing broke, the agent went quiet and we stopped
// waiting.
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

// An approval card posted while the reply is still streaming makes the reply look
// cut off.
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

// Treating a failed Slack write as a terminal step would draw a Slack outage as
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

// A node on a newer build must not abort a live conversation by sending a kind
// this gateway does not know yet.
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

// Settling a step that has not ended the turn would close the reply mid-stream.
func TestEventTranslatorSettleRejectsANonTerminal(t *testing.T) {
	tr, r := newTestEventTranslator()
	if err := tr.Settle(context.Background(), turnStep{Kind: stepContinue}); err == nil {
		t.Fatal("expected settling a non-terminal step to be refused")
	}
	if len(r.calls) != 0 {
		t.Fatalf("nothing may be rendered for a refused settle, got calls: %v", r.calls)
	}
}
