package agent

import (
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock so tool-age assertions are deterministic
// rather than wall-clock dependent.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestToolWatcherTracksInFlightTools(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	w := NewToolWatcher(clk.now)

	if active, _, _ := w.Snapshot(); active != 0 {
		t.Fatalf("empty watcher: active = %d, want 0", active)
	}

	w.Observe("t1", "go test", TaskStatusInProgress)
	clk.advance(90 * time.Second)
	w.Observe("t2", "grep", TaskStatusInProgress)

	active, title, oldest := w.Snapshot()
	if active != 2 {
		t.Fatalf("active = %d, want 2", active)
	}
	if title != "go test" || oldest != 90*time.Second {
		t.Fatalf("oldest = (%q, %s), want (go test, 1m30s)", title, oldest)
	}

	// A terminal status retires the tool.
	w.Observe("t1", "", TaskStatusComplete)
	if active, title, _ = w.Snapshot(); active != 1 || title != "grep" {
		t.Fatalf("after complete: active=%d title=%q, want 1/grep", active, title)
	}

	// The start time is stamped once: a later title-only refinement keeps the age.
	clk.advance(10 * time.Second)
	w.Observe("t2", "grep -r", "")
	if _, title, oldest = w.Snapshot(); title != "grep -r" || oldest != 10*time.Second {
		t.Fatalf("after refine: (%q, %s), want (grep -r, 10s)", title, oldest)
	}
}

// An id-less update has nothing to key on, so it must be dropped rather than
// tracked under "" — otherwise every anonymous update would collapse into one
// phantom tool that never retires and eventually trips the ceiling.
func TestToolWatcherIgnoresEmptyID(t *testing.T) {
	w := NewToolWatcher(nil)
	w.Observe("", "mystery", TaskStatusInProgress)
	if active, _, _ := w.Snapshot(); active != 0 {
		t.Fatalf("active = %d, want 0", active)
	}
}

func TestTaskStatusTerminal(t *testing.T) {
	terminal := []TaskStatus{TaskStatusComplete, TaskStatusFailed, TaskStatusCancelled}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Errorf("%q.Terminal() = false, want true", s)
		}
	}
	// Pending and in-progress keep a tool tracked; so does an unrecognised status,
	// which must not silently retire a tool that is still running.
	live := []TaskStatus{TaskStatusPending, TaskStatusInProgress, "", "something-new"}
	for _, s := range live {
		if s.Terminal() {
			t.Errorf("%q.Terminal() = true, want false", s)
		}
	}
}
