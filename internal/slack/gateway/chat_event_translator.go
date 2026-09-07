package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/slack/alertcard"
)

// eventTranslator turns the ordered event stream a turn emits into calls on a
// chatRenderer, and decides how the turn ends. It is the second of the two
// named translations the gateway/runtime split needs (#188): protocol → Slack
// sink semantics. requestTranslator is the other direction, and the two share
// nothing — this one never sees a Slack message, a file upload or a routing
// decision, and that one never touches a renderer.
//
// # What it consumes, and why it is agent.Event rather than agentwire.Event
//
// The hop from the wire to this codebase's vocabulary already exists and is
// owned elsewhere: agentwire.Decoder turns an agentwire.Event back into an
// agent.Event, deliberately producing "exactly the values the renderer, chat
// handler, session log and delegate runner already consume". The full inbound
// chain is therefore
//
//	agentwire.Event → agentwire.Decoder → agent.Event → eventTranslator → chatRenderer
//
// and this type is the last link. Making it read agentwire.Event instead would
// duplicate the Decoder AND make it unusable by the two in-process drivers that
// exist today (the chat turn loop and the background events router), which is
// precisely how the remote path and the local path would drift apart.
//
// # The layer rule
//
// The renderer's entire observable input is the chatRenderer method set:
// rendered output. It is never handed the event stream and never handed a
// clock, so a liveness detector cannot be attached to it — the mistake #170's
// Concern 4 describes is structurally unavailable rather than merely
// discouraged. This type is the other side of that rule: it sits at the inbound
// event edge, so it owns the liveness window, and it resets that window on
// EVERY event before it decides what (if anything) to render. An EventStatus
// heartbeat is the witness — it keeps the turn alive and produces no renderer
// call at all. See TestEventTranslatorStatusKeepsTurnAliveWithoutRendering.
//
// The half of the rule a test cannot hold is enforced mechanically by the
// `renderclock` analyzer (internal/archtest/renderclockanalyzer, run in CI via
// cmd/archcheck): a chatRenderer implementation may not reference package time.
//
// # Shape
//
// Event-at-a-time, not a loop over a channel, and no goroutine or mutex of its
// own. That is forced by the second driver: backgroundEventsRouter.Handle is a
// callback invoked once per event and cannot hand over a channel. Both drivers
// serialise a session's events already (the chat loop is one goroutine; the
// router holds a mutex across sessions), so this type is used from one
// goroutine at a time and needs no locking.
//
// # What it deliberately does not do
//
// It decides which terminal the turn gets and renders it; it does not carry out
// the POLICY that goes with one. Dropping a wedged session binding, starting a
// credential repair, substituting the error the user is shown, recording the
// journal row and posting the idle aside all stay with the caller, which is why
// Event returns the terminal step and Settle renders it as two separate calls:
// the gap between them is where that policy runs.
//
// Nothing calls this yet. It is Stage 1 of #170 — new code, no callers, the
// serving path untouched. The switch it replaces is ChatHandler.Handle's
// `switch event.Type`; until the wiring stage lands, the two are duplicates and
// the tests below are what keeps them from drifting.
type eventTranslator struct {
	renderer chatRenderer
	logger   *slog.Logger

	// window is how long the stream may go silent before the turn is treated as
	// stalled. A configuration knob rather than a constant (#170 Change F): it
	// is sized against idle gaps, and the keep-alive heartbeat that feeds it is
	// scoped to a turn rather than to a session.
	window time.Duration
	idle   *time.Timer

	reply       strings.Builder
	chunks      int
	bytes       int
	attachments int
	tools       map[string]struct{}
	stopReason  string
}

// turnStepKind names what the caller must do next.
type turnStepKind int

const (
	// stepContinue — the event was rendered (or deliberately not); keep reading.
	stepContinue turnStepKind = iota
	// stepPermission — the agent is asking a human to approve a tool call. The
	// reply has already been settled through the renderer so the approval card
	// lands below a committed message rather than an unfinished stream; the
	// caller asks the human and answers the prompt's channel. Not terminal.
	stepPermission
	// stepFinished — the turn ended successfully, either with an explicit
	// EventComplete or by the stream closing with no terminal event at all.
	stepFinished
	// stepFailed — the turn ended with an agent error that is not a
	// cancellation.
	stepFailed
	// stepInterrupted — the turn was cancelled by the caller. Rendered as the
	// "_interrupted_" marker, never as a failure card: the agent did not fail.
	stepInterrupted
	// stepStalled — the liveness window elapsed with no event.
	stepStalled
)

// String names the step, so a log line or a failed assertion reads as
// "interrupted" rather than "4".
func (k turnStepKind) String() string {
	switch k {
	case stepContinue:
		return "continue"
	case stepPermission:
		return "permission"
	case stepFinished:
		return "finished"
	case stepFailed:
		return "failed"
	case stepInterrupted:
		return "interrupted"
	case stepStalled:
		return "stalled"
	default:
		return "unknown"
	}
}

// terminal reports whether this step ends the turn, meaning the caller applies
// its policy and then calls Settle exactly once.
func (k turnStepKind) terminal() bool {
	switch k {
	case stepFinished, stepFailed, stepInterrupted, stepStalled:
		return true
	default:
		return false
	}
}

// turnStep is the translator's answer to one event: what kind of step it was,
// and the facts the caller needs to apply policy before the terminal renders.
type turnStep struct {
	Kind turnStepKind
	// Err is the agent's error, carried by VALUE so its identity survives —
	// every consumer on this path compares it with errors.Is, and the wire
	// protocol carries discriminants for exactly that reason (#187). The caller
	// may replace it before Settle (the credential-repair path substitutes
	// errCredentialBlocked for the backend's raw "/login" prose).
	Err error
	// ToolCeiling reports that Err is agent.ErrToolCeiling. Surfaced as a flag
	// rather than left to the caller to re-test, because the action it implies —
	// drop the session binding, the backend cannot be told to abandon the call —
	// is the caller's, while noticing it is a property of the stream.
	ToolCeiling bool
	// StopReason is the agent's reported reason for ending the turn, present on
	// a stepFinished that came from an explicit EventComplete.
	StopReason string
	// Permission is the prompt to put to a human, set only on stepPermission.
	// The translator never answers it: who may approve, on which Slack surface,
	// and what a missing asker means are gateway policy, not translation.
	Permission *agent.PermissionPrompt
}

// turnStats is the read-only view of what the turn produced, accumulated as the
// events went past. The terminal decision reads it (an attachment-only turn is
// not an empty reply), and so does the caller's journal row — which is why it
// is gathered here rather than by a second switch over the same events.
type turnStats struct {
	// Reply is the agent's reply text, concatenated in stream order.
	Reply string
	// Chunks counts non-empty text events; Bytes their total length.
	Chunks int
	Bytes  int
	// Attachments counts files DELIVERED, not files offered: a failed upload is
	// not counted, so an attachment-only turn whose upload failed still surfaces
	// the empty-reply note instead of ending in silence.
	Attachments int
	// Tools counts distinct tool ids seen this turn. Plan entries are excluded —
	// they are the agent's to-do list, not work it ran.
	Tools      int
	StopReason string
}

// newEventTranslator builds a translator that drives renderer, treating a
// silence longer than window as a stall. A non-positive window falls back to
// defaultIdleTimeout, matching ChatHandler.effectiveIdleTimeout.
//
// The liveness timer is armed immediately: the window between the prompt being
// sent and the first event is exactly as much a silence as any other. Callers
// must StopLiveness when the turn ends.
func newEventTranslator(renderer chatRenderer, window time.Duration, logger *slog.Logger) *eventTranslator {
	if logger == nil {
		logger = slog.Default()
	}
	if window <= 0 {
		window = defaultIdleTimeout
	}
	return &eventTranslator{
		renderer: renderer,
		logger:   logger,
		window:   window,
		idle:     time.NewTimer(window),
		tools:    map[string]struct{}{},
	}
}

// Idle fires when the stream has been silent for a whole window. A caller that
// selects over an event channel waits on it directly; a push-driven caller (the
// background events router, which is handed one event per call and has no loop
// to select in) waits on it from a goroutine of its own — the reset and disarm
// calls are the same either way.
func (t *eventTranslator) Idle() <-chan time.Time { return t.idle.C }

// StopLiveness disarms the window. Idempotent; safe in a defer.
func (t *eventTranslator) StopLiveness() { t.idle.Stop() }

// Stats reports what the turn has produced so far.
func (t *eventTranslator) Stats() turnStats {
	return turnStats{
		Reply:       t.reply.String(),
		Chunks:      t.chunks,
		Bytes:       t.bytes,
		Attachments: t.attachments,
		Tools:       len(t.tools),
		StopReason:  t.stopReason,
	}
}

// Event translates one event into renderer calls and reports what the caller
// must do next.
//
// The liveness window is reset FIRST, before the event's kind is even looked at.
// That ordering is the whole point of the heartbeat: an EventStatus tick is
// never rendered, and if the reset lived inside the switch it would keep alive
// only the turns that happened to be producing output.
//
// A non-nil error is a DELIVERY failure — Slack refused a write — and ends the
// turn without a terminal step: the caller returns it and its deferred
// EnsureStopped seals whatever is open. A failure the agent reported arrives as
// an event instead and comes back as stepFailed.
func (t *eventTranslator) Event(ctx context.Context, ev agent.Event) (turnStep, error) {
	resetIdleTimer(t.idle, t.window)

	switch ev.Type {
	case agent.EventText:
		if ev.Text != "" {
			t.chunks++
			t.bytes += len(ev.Text)
			t.reply.WriteString(ev.Text)
		}
		if err := t.renderer.Text(ctx, ev.Text); err != nil {
			return turnStep{Kind: stepContinue}, err
		}
		return turnStep{Kind: stepContinue}, nil

	case agent.EventStatus:
		// Progress/meta only — compaction, an empty-reply retry, or a
		// tool-heartbeat keep-alive from either backend. Never part of the reply,
		// so nothing is rendered. The timer was already reset above, which is the
		// entire contribution of a heartbeat: it keeps a long tool's turn alive
		// without the renderer ever learning that time passed.
		return turnStep{Kind: stepContinue}, nil

	case agent.EventTask:
		if ev.Task == nil {
			return turnStep{Kind: stepContinue}, nil
		}
		// Plan entries are the agent's task list, not work it ran — only tool
		// calls count towards what an otherwise-empty turn actually did.
		if ev.Task.Kind != agent.TaskKindPlan && ev.Task.ID != "" {
			t.tools[ev.Task.ID] = struct{}{}
		}
		if err := t.renderer.Task(ctx, ev.Task); err != nil {
			return turnStep{Kind: stepContinue}, err
		}
		return turnStep{Kind: stepContinue}, nil

	case agent.EventAttachment:
		if ev.Attachment == nil {
			return turnStep{Kind: stepContinue}, nil
		}
		// Best-effort by design: the text reply still matters, so a failed upload
		// is recorded and the turn continues rather than ending on it.
		if err := t.renderer.Attachment(ctx, ev.Attachment); err != nil {
			t.logger.Warn("failed to deliver agent attachment", "filename", ev.Attachment.Filename, "error", err)
		} else {
			t.attachments++
		}
		return turnStep{Kind: stepContinue}, nil

	case agent.EventPermission:
		if ev.Permission == nil {
			return turnStep{Kind: stepContinue}, nil
		}
		// Settle the open reply before the caller posts anything out of band, so
		// the approval card lands below a committed message instead of an
		// unfinished, streaming one — the "looks truncated" symptom. This mirrors
		// the native loop, whose inline approval is naturally ordered after the
		// tool's task event. Ordering is a rendering decision and belongs here;
		// who is asked, and what a missing asker means, does not.
		t.renderer.BeginInterjection(ctx)
		return turnStep{Kind: stepPermission, Permission: ev.Permission}, nil

	case agent.EventError:
		// A caller interrupt (a new message, or /stop) surfaces here as a context
		// cancellation rather than an agent failure. Both shapes are checked: the
		// interrupt closure asks the session to stop and only cancels ctx after a
		// grace period, so the agent's aborted-turn event routinely wins that
		// race. They are the same event — the turn was cancelled — and must
		// render the same marker.
		if errors.Is(ev.Error, context.Canceled) || errors.Is(context.Cause(ctx), context.Canceled) {
			return turnStep{Kind: stepInterrupted, Err: ev.Error}, nil
		}
		return turnStep{
			Kind: stepFailed,
			Err:  ev.Error,
			// Matched on the backend-neutral sentinel: any backend that grows a
			// ceiling gets this handling without touching the relay.
			ToolCeiling: errors.Is(ev.Error, agent.ErrToolCeiling),
		}, nil

	case agent.EventComplete:
		t.stopReason = ev.StopReason
		return turnStep{Kind: stepFinished, StopReason: ev.StopReason}, nil
	}

	// An unknown kind. Ignored rather than fatal, matching the switch in
	// ChatHandler.Handle: a node running a newer build must not be able to abort
	// a live conversation by emitting a kind this gateway has not learnt yet.
	// The wire codec is where an unknown kind IS an error, because there it is
	// still recoverable (#187).
	t.logger.Debug("ignoring unknown agent event kind", "kind", string(ev.Type))
	return turnStep{Kind: stepContinue}, nil
}

// Closed is the step for a stream that ended without an explicit terminal event.
// It is a successful finish: the same treatment ChatHandler.Handle gives a
// closed channel, so a backend that simply stops talking still gets its reply
// sealed and its empty-reply note.
func (t *eventTranslator) Closed() turnStep { return turnStep{Kind: stepFinished} }

// Stalled is the step for a liveness window that elapsed. It is deliberately
// NOT a failure: nothing broke, the agent went quiet and we stopped waiting, so
// tool blocks keep their last reported state and nothing is painted red.
func (t *eventTranslator) Stalled() turnStep { return turnStep{Kind: stepStalled} }

// Settle renders the turn's terminal, exactly once, after the caller has applied
// whatever policy the step called for. It reports only the renderer's own
// delivery error.
func (t *eventTranslator) Settle(ctx context.Context, step turnStep) error {
	switch step.Kind {
	case stepFinished:
		return t.renderer.Finish(ctx, t.emptyReply())
	case stepFailed:
		return t.renderer.Fail(ctx, step.Err)
	case stepInterrupted:
		t.renderer.Interrupted(ctx)
		return nil
	case stepStalled:
		t.renderer.EnsureStopped(ctx)
		return nil
	default:
		return fmt.Errorf("settle: %v is not a terminal step", step.Kind)
	}
}

// emptyReply is the alert a successful turn carries when it produced no reply,
// or nil when it produced one.
//
// A turn that delivered a file but no prose is NOT empty: suppressing the note
// there is what stops an attachment-only reply looking like a failed turn.
func (t *eventTranslator) emptyReply() *alertcard.Spec {
	if t.bytes > 0 || t.attachments > 0 {
		return nil
	}
	spec := emptyReplySpec(len(t.tools))
	return &spec
}
