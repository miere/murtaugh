package agent

import (
	"errors"
	"sync"
	"time"
)

const (
	// DefaultToolHeartbeatInterval is how often a backend re-announces that a
	// still-running tool call is alive. The gateway's idle watchdog resets on any
	// event, so without this a long, output-silent tool reads as a stalled turn.
	//
	// It lives here rather than in each backend because the three backends must
	// behave identically from the user's seat: a tool that survives ten minutes on
	// one backend cannot be reaped at ninety seconds on another.
	DefaultToolHeartbeatInterval = 30 * time.Second

	// DefaultToolCeiling bounds how long a single tool call may hold a turn. The
	// heartbeat suppresses the idle watchdog while a tool runs, so without a
	// ceiling a genuinely wedged tool (waiting on stdin, deadlocked, blocked on a
	// human who walked away) would hold the turn forever.
	DefaultToolCeiling = time.Hour
)

// ErrToolCeiling marks a turn aborted because one tool ran past its execution
// ceiling without producing a result.
//
// Backends wrap it; the Slack relay matches on it with errors.Is to drop the
// session binding, because a backend that cannot cancel an in-flight tool has no
// way to tell the agent to stop. That match is why the error is declared here and
// not per-backend: a claudecode ceiling and an ACP ceiling are the same event to
// the user, so they must be the same error to the code that renders them.
var ErrToolCeiling = errors.New("tool exceeded its execution ceiling")

// ToolWatcher tracks which of a session's tool calls are currently in flight, so
// a heartbeat can keep a long tool's turn alive and a ceiling can fail a wedged
// one. A tool announces itself with a non-terminal task status and is retired by
// a terminal one; between the two it produces no events of its own.
//
// It is safe for concurrent use: the stream-reading goroutine observes while the
// heartbeat goroutine snapshots.
type ToolWatcher struct {
	mu      sync.Mutex
	running map[string]runningTool
	now     func() time.Time
}

type runningTool struct {
	started time.Time
	title   string
}

// NewToolWatcher builds an empty watcher. A nil now uses time.Now; tests inject a
// fake clock to drive the ceiling without waiting on one.
func NewToolWatcher(now func() time.Time) *ToolWatcher {
	if now == nil {
		now = time.Now
	}
	return &ToolWatcher{running: make(map[string]runningTool), now: now}
}

// Observe folds one task update into the in-flight set: a terminal status retires
// the tool, any other status (in_progress, pending, or a title-only refinement)
// starts or keeps tracking it.
//
// The start time is stamped once, on first sighting, so the ceiling measures a
// tool's true age across its later refinements rather than restarting on each.
func (w *ToolWatcher) Observe(id, title string, status TaskStatus) {
	if id == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if status.Terminal() {
		delete(w.running, id)
		return
	}
	r, ok := w.running[id]
	if !ok {
		r.started = w.now()
	}
	if title != "" {
		r.title = title
	}
	w.running[id] = r
}

// Snapshot reports how many tools are in flight and, for the longest-running one,
// its title and age — the inputs a heartbeat needs to choose between a keep-alive
// and the ceiling.
func (w *ToolWatcher) Snapshot() (active int, oldestTitle string, oldest time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	active = len(w.running)
	var earliest time.Time
	for _, r := range w.running {
		if earliest.IsZero() || r.started.Before(earliest) {
			earliest = r.started
			oldestTitle = r.title
		}
	}
	if !earliest.IsZero() {
		oldest = w.now().Sub(earliest)
	}
	return active, oldestTitle, oldest
}

// Terminal reports whether s ends a tool call, so the watcher can retire it.
func (s TaskStatus) Terminal() bool {
	switch s {
	case TaskStatusComplete, TaskStatusFailed, TaskStatusCancelled:
		return true
	default:
		return false
	}
}
