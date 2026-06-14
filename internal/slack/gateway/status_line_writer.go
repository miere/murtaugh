package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh-dev-toolkit/internal/acp"
)

// progressRenderer is the surface ChatHandler uses to turn ACP task events into
// Slack progress UI. Two implementations back it: TaskCardWriter (the full
// multi-card plan) and StatusLineWriter (the simplified single line). The
// handler is written against this interface so the choice is a per-agent config
// concern, not a code path.
type progressRenderer interface {
	UpdateFromEvent(ctx context.Context, event *acp.TaskEvent) error
	Complete(ctx context.Context, taskID, title string) error
	Fail(ctx context.Context, taskID, title string) error
}

// statusLineTaskID is the single, fixed task id every StatusLineWriter update
// reuses. Because Slack upserts task cards by id, reusing one id makes every
// write overwrite the previous one in place — the last-write-wins behaviour
// that collapses an entire turn's activity into a single line.
const statusLineTaskID = "murtaugh_progress"

// defaultStatusLineTitle labels the line before any task has reported a title,
// and is the fallback when an update carries none.
const defaultStatusLineTitle = "Working…"

// StatusLineWriter renders an agent's progress as a single task card that is
// rewritten on every update (last-write-wins) and resolved to a check when the
// turn ends. It shares the StreamWriter — and therefore the Slack message —
// with the streamed answer, and relies on Timeline display mode so its lone
// card renders without a Plan header. Compared with TaskCardWriter it keeps no
// per-task state: there is exactly one line, so there is nothing to finalise
// beyond a single terminal write.
type StatusLineWriter struct {
	api      StreamAPI
	streamer *StreamWriter
	logger   *slog.Logger
	interval time.Duration

	mu        sync.Mutex
	lastTitle string
	lastFlush time.Time
	flushed   bool
	done      bool
}

// NewStatusLineWriter creates a simplified progress writer that posts to the
// same Slack stream as streamer.
func NewStatusLineWriter(api StreamAPI, streamer *StreamWriter, interval time.Duration, logger *slog.Logger) *StatusLineWriter {
	if interval <= 0 {
		interval = defaultTaskUpdateInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &StatusLineWriter{api: api, streamer: streamer, logger: logger, interval: interval}
}

// UpdateFromEvent collapses a task event into the single status line. The
// event's title (or the last one seen, or a default) becomes the line's text.
// Non-terminal updates are throttled so a rapidly-iterating agent does not
// hammer Slack; the latest title is always remembered so the eventual terminal
// write reflects it.
func (w *StatusLineWriter) UpdateFromEvent(ctx context.Context, event *acp.TaskEvent) error {
	if event == nil {
		return nil
	}
	title := w.rememberTitle(event.Title)
	if !w.shouldFlush() {
		return nil
	}
	return w.send(ctx, title, slack.TaskCardStatusInProgress)
}

// Complete resolves the status line to a check. It fires at most once per turn
// (the handler may call it for every still-running task); the title argument,
// the last seen title, or a default is used, in that order.
func (w *StatusLineWriter) Complete(ctx context.Context, _, title string) error {
	return w.finish(ctx, title, slack.TaskCardStatusComplete)
}

// Fail resolves the status line to an error state, once per turn.
func (w *StatusLineWriter) Fail(ctx context.Context, _, title string) error {
	return w.finish(ctx, title, slack.TaskCardStatusError)
}

func (w *StatusLineWriter) finish(ctx context.Context, title string, status slack.TaskCardStatus) error {
	w.mu.Lock()
	if w.done {
		w.mu.Unlock()
		return nil
	}
	w.done = true
	w.mu.Unlock()
	return w.send(ctx, w.rememberTitle(title), status)
}

// send writes the single task card, starting the stream if it has not begun.
func (w *StatusLineWriter) send(ctx context.Context, title string, status slack.TaskCardStatus) error {
	chunk := slack.NewTaskUpdateChunk(statusLineTaskID, title)
	chunk.Status = status
	startedAt := time.Now()
	if !w.streamer.Started() {
		if err := w.streamer.StartWithOptions(ctx, slack.MsgOptionChunks(chunk)); err != nil {
			return fmt.Errorf("start stream with status line: %w", err)
		}
		w.recordFlush(startedAt)
		return nil
	}
	if _, _, err := w.api.AppendStreamContext(ctx, w.streamer.StreamChannel(), w.streamer.StreamTS(), slack.MsgOptionChunks(chunk)); err != nil {
		return fmt.Errorf("append status line: %w", err)
	}
	w.recordFlush(startedAt)
	return nil
}

// rememberTitle records a non-empty title and returns the best label for the
// line: the supplied title, else the last-seen title, else the default.
func (w *StatusLineWriter) rememberTitle(title string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if title != "" {
		w.lastTitle = title
	}
	if w.lastTitle == "" {
		return defaultStatusLineTitle
	}
	return w.lastTitle
}

// shouldFlush rate-limits non-terminal updates: the first always flushes, the
// rest only once the interval has elapsed since the last write.
func (w *StatusLineWriter) shouldFlush() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.flushed {
		return true
	}
	return time.Since(w.lastFlush) >= w.interval
}

func (w *StatusLineWriter) recordFlush(t time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushed = true
	w.lastFlush = t
}
