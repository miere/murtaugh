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

type eventTranslator struct {
	renderer chatRenderer
	logger   *slog.Logger

	window time.Duration
	idle   *time.Timer

	reply       strings.Builder
	chunks      int
	bytes       int
	attachments int
	tools       map[string]struct{}
	stopReason  string
}

type turnStepKind int

const (
	stepContinue turnStepKind = iota
	stepPermission
	stepFinished
	stepFailed
	stepInterrupted
	stepStalled
)

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

func (k turnStepKind) terminal() bool {
	switch k {
	case stepFinished, stepFailed, stepInterrupted, stepStalled:
		return true
	default:
		return false
	}
}

type turnStep struct {
	Kind        turnStepKind
	Err         error
	ToolCeiling bool
	StopReason  string
	Permission  *agent.PermissionPrompt
}

type turnStats struct {
	Reply       string
	Chunks      int
	Bytes       int
	Attachments int
	Tools       int
	StopReason  string
}

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

func (t *eventTranslator) Idle() <-chan time.Time { return t.idle.C }

func (t *eventTranslator) StopLiveness() { t.idle.Stop() }

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
		return turnStep{Kind: stepContinue}, nil

	case agent.EventTask:
		if ev.Task == nil {
			return turnStep{Kind: stepContinue}, nil
		}
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
		t.renderer.BeginInterjection(ctx)
		return turnStep{Kind: stepPermission, Permission: ev.Permission}, nil

	case agent.EventError:
		if errors.Is(ev.Error, context.Canceled) || errors.Is(context.Cause(ctx), context.Canceled) {
			return turnStep{Kind: stepInterrupted, Err: ev.Error}, nil
		}
		return turnStep{
			Kind:        stepFailed,
			Err:         ev.Error,
			ToolCeiling: errors.Is(ev.Error, agent.ErrToolCeiling),
		}, nil

	case agent.EventComplete:
		t.stopReason = ev.StopReason
		return turnStep{Kind: stepFinished, StopReason: ev.StopReason}, nil
	}

	t.logger.Debug("ignoring unknown agent event kind", "kind", string(ev.Type))
	return turnStep{Kind: stepContinue}, nil
}

func (t *eventTranslator) Closed() turnStep { return turnStep{Kind: stepFinished} }

func (t *eventTranslator) Stalled() turnStep { return turnStep{Kind: stepStalled} }

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

func (t *eventTranslator) emptyReply() *alertcard.Spec {
	if t.bytes > 0 || t.attachments > 0 {
		return nil
	}
	spec := emptyReplySpec(len(t.tools))
	return &spec
}
