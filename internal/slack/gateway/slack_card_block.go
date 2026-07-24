package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh/internal/agent"
)

// bufferedCardBlock renders tasks-mode tool cards on a surface that cannot stream
// (a canvas): the same Plan/Task-card blocks the streaming path emits as chunks are
// instead posted as a single message (chat.postMessage with a PlanBlock) and edited
// in place (chat.update) as statuses change. The only visible difference from
// streaming is Slack's small "(edited)" label — so tasks mode keeps its real cards
// on a canvas rather than regressing to a plain status line (spec 021 §2, no-UX-
// regression). It is a toolBlock, so sectionRenderer drives it identically to
// cardToolBlock.
type bufferedCardBlock struct {
	messenger statusMessenger
	channelID string
	threadTS  string
	interval  time.Duration
	logger    *slog.Logger

	planTitle string
	order     []string // task ids in first-seen order
	titles    map[string]string
	statuses  map[string]slack.TaskCardStatus
	running   map[string]bool // ids not yet in a terminal state, for FinishWith

	posted    bool
	msgTS     string
	lastFlush time.Time
	stopped   bool
}

func newBufferedCardBlock(messenger statusMessenger, channelID, threadTS string, opts StreamWriterOptions, logger *slog.Logger) *bufferedCardBlock {
	if logger == nil {
		logger = slog.Default()
	}
	return &bufferedCardBlock{
		messenger: messenger,
		channelID: channelID,
		threadTS:  threadTS,
		interval:  defaultTaskUpdateInterval,
		logger:    logger,
		planTitle: defaultPlanTitle,
		titles:    map[string]string{},
		statuses:  map[string]slack.TaskCardStatus{},
		running:   map[string]bool{},
	}
}

// UpdateFromEvent folds a task event into the tracked plan and re-renders the
// message. Non-terminal updates are throttled (the streaming path throttles for the
// same reason — avoid hammering chat.update); the first render and every terminal
// status always paint.
func (b *bufferedCardBlock) UpdateFromEvent(ctx context.Context, ev *agent.TaskEvent) error {
	if ev == nil {
		return nil
	}
	id := ev.ID
	if _, seen := b.titles[id]; !seen && !b.hasStatus(id) {
		b.order = append(b.order, id)
	}
	b.titles[id] = b.titleFor(id, ev.Title)
	status := b.resolveStatus(id, ev.Status)
	b.statuses[id] = status
	b.running[id] = !isTerminalCardStatus(status)

	if !b.shouldRender(status) {
		return nil
	}
	return b.render(ctx, false)
}

// FinishWith resolves any still-running card to complete and paints the final
// state, then stops. There is no stream to close — the message is already posted.
func (b *bufferedCardBlock) FinishWith(ctx context.Context, _ string) error {
	if b.stopped {
		return nil
	}
	b.stopped = true
	if len(b.order) == 0 {
		return nil
	}
	return b.render(ctx, true)
}

func (b *bufferedCardBlock) hasStatus(id string) bool { _, ok := b.statuses[id]; return ok }

func (b *bufferedCardBlock) titleFor(id, title string) string {
	if title != "" {
		return title
	}
	if t := b.titles[id]; t != "" {
		return t
	}
	return defaultTaskTitle
}

// resolveStatus mirrors TaskCardWriter.resolveStatus: an update that carries no
// recognised status keeps the card's previous state (ACP title-only ticks must not
// flip a completed card back to a spinner), defaulting to in_progress only on the
// first sighting.
func (b *bufferedCardBlock) resolveStatus(id string, s agent.TaskStatus) slack.TaskCardStatus {
	if mapped, ok := knownTaskStatus(s); ok {
		return mapped
	}
	if prev, ok := b.statuses[id]; ok {
		return prev
	}
	return slack.TaskCardStatusInProgress
}

func (b *bufferedCardBlock) shouldRender(status slack.TaskCardStatus) bool {
	if !b.posted || isTerminalCardStatus(status) {
		return true
	}
	return time.Since(b.lastFlush) >= b.interval
}

// render posts the PlanBlock the first time and edits it in place thereafter. When
// finalizing, every still-running card is shown complete so nothing is stranded
// mid-spinner.
func (b *bufferedCardBlock) render(ctx context.Context, finalizing bool) error {
	if b.messenger == nil {
		b.logger.Warn("buffered card block has no messenger; dropping task cards", "channel", b.channelID)
		return nil
	}
	block := b.planBlock(finalizing)
	if !b.posted {
		options := []slack.MsgOption{slack.MsgOptionBlocks(block)}
		if b.threadTS != "" {
			options = append(options, slack.MsgOptionTS(b.threadTS))
		}
		_, ts, err := b.messenger.PostMessageContext(ctx, b.channelID, options...)
		if err != nil {
			return fmt.Errorf("post task cards: %w", err)
		}
		b.posted = true
		b.msgTS = ts
		b.lastFlush = time.Now()
		return nil
	}
	if _, _, _, err := b.messenger.UpdateMessageContext(ctx, b.channelID, b.msgTS, slack.MsgOptionBlocks(block)); err != nil {
		return fmt.Errorf("update task cards: %w", err)
	}
	b.lastFlush = time.Now()
	return nil
}

func (b *bufferedCardBlock) planBlock(finalizing bool) *slack.PlanBlock {
	tasks := make([]*slack.TaskCardBlock, 0, len(b.order))
	for _, id := range b.order {
		status := b.statuses[id]
		if finalizing && b.running[id] {
			status = slack.TaskCardStatusComplete
		}
		tasks = append(tasks, slack.NewTaskCardBlock(id, b.titles[id]).WithStatus(status))
	}
	return slack.NewPlanBlock(b.planTitle).WithTasks(tasks...)
}

func isTerminalCardStatus(s slack.TaskCardStatus) bool {
	return s == slack.TaskCardStatusComplete || s == slack.TaskCardStatusError
}

// --- defaultCardBlock (negotiating tool block) ------------------------------

// defaultCardBlock streams tool cards, and downgrades once to buffered PlanBlock
// posting the first time the surface rejects the stream (a canvas). Because the
// downgrade can only fire on the first stream open (the first UpdateFromEvent),
// before any card has rendered, re-running that one event on the buffered block
// loses nothing. The negotiation is independent of the reply-text sink's — both
// discover the same surface separately and reach the same buffered outcome; a
// shared per-turn probe (spec 021 §7) would remove the second doomed stream-open
// but is not needed for correctness.
type defaultCardBlock struct {
	streaming   toolBlock
	newBuffered func() toolBlock
	active      toolBlock
	downgraded  bool
	logger      *slog.Logger
}

func newDefaultCardBlock(api StreamAPI, messenger statusMessenger, channelID, threadTS string, opts StreamWriterOptions, logger *slog.Logger) *defaultCardBlock {
	if logger == nil {
		logger = slog.Default()
	}
	return &defaultCardBlock{
		streaming:   newCardToolBlock(api, channelID, opts, logger),
		newBuffered: func() toolBlock { return newBufferedCardBlock(messenger, channelID, threadTS, opts, logger) },
		logger:      logger,
	}
}

func (c *defaultCardBlock) current() toolBlock {
	if c.active == nil {
		c.active = c.streaming
	}
	return c.active
}

func (c *defaultCardBlock) UpdateFromEvent(ctx context.Context, ev *agent.TaskEvent) error {
	err := c.current().UpdateFromEvent(ctx, ev)
	if err != nil && !c.downgraded && isChannelTypeUnsupported(err) {
		c.downgrade()
		return c.active.UpdateFromEvent(ctx, ev)
	}
	return err
}

func (c *defaultCardBlock) FinishWith(ctx context.Context, done string) error {
	return c.current().FinishWith(ctx, done)
}

func (c *defaultCardBlock) downgrade() {
	c.active = c.newBuffered()
	c.downgraded = true
	c.logger.Info("downgraded task cards to buffered PlanBlock post (surface cannot stream)")
}
