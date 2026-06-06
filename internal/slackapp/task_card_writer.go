package slackapp

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh-dev-toolkit/internal/acp"
)

const defaultTaskUpdateInterval = 1 * time.Second

// TaskCardWriter sends task-card updates as Slack stream chunks alongside a
// StreamWriter. Each task update is rate-limited so we do not hammer the Slack
// API while the agent is rapidly iterating.
type TaskCardWriter struct {
	api       StreamAPI
	streamer  *StreamWriter
	logger    *slog.Logger
	interval  time.Duration
	mu        sync.Mutex
	lastFlush map[string]time.Time
	titles    map[string]string
}

// NewTaskCardWriter creates a writer that posts task-card updates to the same
// Slack stream as streamer.
func NewTaskCardWriter(api StreamAPI, streamer *StreamWriter, interval time.Duration, logger *slog.Logger) *TaskCardWriter {
	if interval <= 0 {
		interval = defaultTaskUpdateInterval
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &TaskCardWriter{
		api:       api,
		streamer:  streamer,
		logger:    logger,
		interval:  interval,
		lastFlush: make(map[string]time.Time),
		titles:    make(map[string]string),
	}
}

// Update sends a task-card update for a single task. The stream is started if
// needed. Updates are suppressed when the same task was flushed recently,
// unless the status is a terminal state (complete or error) in which case the
// update is always sent.
func (w *TaskCardWriter) Update(ctx context.Context, taskID, title string, status slack.TaskCardStatus) error {
	if !w.shouldFlush(taskID, status) {
		return nil
	}
	chunk := slack.NewTaskUpdateChunk(taskID, title)
	chunk.Status = status
	startedAt := time.Now()
	if !w.streamer.Started() {
		if err := w.streamer.StartWithOptions(ctx, slack.MsgOptionChunks(chunk)); err != nil {
			return fmt.Errorf("start stream with task update: %w", err)
		}
		w.recordFlush(taskID, startedAt)
		w.logger.Info("started stream with task update", "task_id", taskID, "status", status, "duration", time.Since(startedAt))
		return nil
	}
	_, _, err := w.api.AppendStreamContext(ctx, w.streamer.StreamChannel(), w.streamer.StreamTS(), slack.MsgOptionChunks(chunk))
	if err != nil {
		return fmt.Errorf("append task update chunk: %w", err)
	}
	w.recordFlush(taskID, startedAt)
	w.logger.Info("sent task update", "task_id", taskID, "status", status, "duration", time.Since(startedAt))
	return nil
}

// Fail marks any running task as failed and sends the update.
func (w *TaskCardWriter) Fail(ctx context.Context, taskID, title string) error {
	return w.Update(ctx, taskID, title, slack.TaskCardStatusError)
}

// Complete marks a task as completed and sends the update.
func (w *TaskCardWriter) Complete(ctx context.Context, taskID, title string) error {
	return w.Update(ctx, taskID, title, slack.TaskCardStatusComplete)
}

// UpdateFromEvent maps an ACP task event to a Slack task update and sends it.
func (w *TaskCardWriter) UpdateFromEvent(ctx context.Context, event *acp.TaskEvent) error {
	if event == nil {
		return nil
	}
	status := mapTaskStatus(event.Status)
	title := w.titleFor(event.ID, event.Title)
	if title == "" {
		title = "Tool call"
	}
	if err := w.Update(ctx, event.ID, title, status); err != nil {
		return err
	}
	return nil
}

func (w *TaskCardWriter) titleFor(taskID, title string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if title != "" {
		w.titles[taskID] = title
		return title
	}
	return w.titles[taskID]
}

func (w *TaskCardWriter) shouldFlush(taskID string, status slack.TaskCardStatus) bool {
	if status == slack.TaskCardStatusComplete || status == slack.TaskCardStatusError {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	last, ok := w.lastFlush[taskID]
	if !ok {
		return true
	}
	return time.Since(last) >= w.interval
}

func (w *TaskCardWriter) recordFlush(taskID string, t time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastFlush[taskID] = t
}

func mapTaskStatus(status acp.TaskStatus) slack.TaskCardStatus {
	switch status {
	case acp.TaskStatusPending:
		return slack.TaskCardStatusPending
	case acp.TaskStatusInProgress:
		return slack.TaskCardStatusInProgress
	case acp.TaskStatusComplete:
		return slack.TaskCardStatusComplete
	case acp.TaskStatusFailed:
		return slack.TaskCardStatusError
	case acp.TaskStatusCancelled:
		return slack.TaskCardStatusError
	default:
		return slack.TaskCardStatusInProgress
	}
}
