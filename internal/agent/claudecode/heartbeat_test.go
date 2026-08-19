package claudecode

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

// fakeClock is a manually-advanced clock so ceiling assertions are deterministic
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

// newHeartbeatSession builds a session and an open subscription without spawning
// a process, so the heartbeat can be driven directly.
func newHeartbeatSession(t *testing.T, opts Options) (*procSession, *subscription) {
	t.Helper()
	opts.Command = "unused"
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := newProcSession("test-session", opts)
	sub := &subscription{
		events:  make(chan agent.Event, 16),
		watcher: agent.NewToolWatcher(s.now),
		hbStop:  make(chan struct{}),
		hbDone:  make(chan struct{}),
	}
	s.mu.Lock()
	s.active = sub
	s.procDone = make(chan struct{})
	s.mu.Unlock()
	return s, sub
}

// TestHeartbeatKeepsATurnAliveWhileAToolRuns is the whole point of the file: a
// tool in flight produces keep-alive events, so the gateway's idle watchdog (which
// resets on any event) does not abandon a turn that is legitimately waiting.
func TestHeartbeatKeepsATurnAliveWhileAToolRuns(t *testing.T) {
	s, sub := newHeartbeatSession(t, Options{ToolHeartbeatInterval: time.Millisecond})
	sub.watcher.Observe("t1", "AskUserQuestion", agent.TaskStatusInProgress)

	go s.heartbeat(sub)
	defer sub.stopHeartbeat()

	select {
	case ev := <-sub.events:
		if ev.Type != agent.EventStatus || ev.Text != heartbeatText {
			t.Fatalf("got %+v, want an EventStatus %q", ev, heartbeatText)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no keep-alive emitted while a tool was in flight")
	}
}

// An idle turn must still trip the watchdog: the heartbeat suppresses it only for
// tools, never for an agent that has simply gone quiet.
func TestHeartbeatStaysSilentWithNoToolInFlight(t *testing.T) {
	s, sub := newHeartbeatSession(t, Options{ToolHeartbeatInterval: time.Millisecond})

	go s.heartbeat(sub)
	defer sub.stopHeartbeat()

	select {
	case ev := <-sub.events:
		t.Fatalf("emitted %+v with no tool in flight; the turn must be allowed to go idle", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// A retired tool stops the keep-alives, so a turn that finishes its tool and then
// stalls is still caught by the watchdog.
func TestHeartbeatStopsAfterToolCompletes(t *testing.T) {
	s, sub := newHeartbeatSession(t, Options{ToolHeartbeatInterval: time.Millisecond})
	sub.watcher.Observe("t1", "Bash", agent.TaskStatusInProgress)

	go s.heartbeat(sub)
	defer sub.stopHeartbeat()

	select {
	case <-sub.events:
	case <-time.After(2 * time.Second):
		t.Fatal("expected a keep-alive while the tool ran")
	}

	sub.watcher.Observe("t1", "", agent.TaskStatusComplete)
	// Drain whatever was already queued before the retirement landed.
	time.Sleep(20 * time.Millisecond)
	for len(sub.events) > 0 {
		<-sub.events
	}
	select {
	case ev := <-sub.events:
		t.Fatalf("emitted %+v after the tool completed", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestHeartbeatCeilingFailsTheTurn covers the backstop: the heartbeat suppresses
// the idle watchdog, so without a ceiling a wedged tool would hold a turn forever.
func TestHeartbeatCeilingFailsTheTurn(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	s, sub := newHeartbeatSession(t, Options{
		ToolHeartbeatInterval: time.Millisecond,
		ToolCeiling:           time.Hour,
		Now:                   clk.now,
	})
	sub.watcher.Observe("t1", "wedged-tool", agent.TaskStatusInProgress)
	clk.advance(2 * time.Hour)

	go s.heartbeat(sub)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-sub.events:
			if !ok {
				t.Fatal("channel closed without an error event")
			}
			if ev.Type != agent.EventError {
				continue // a keep-alive may have raced ahead of the ceiling tick
			}
			if !errors.Is(ev.Error, agent.ErrToolCeiling) {
				t.Fatalf("error = %v, want it to wrap agent.ErrToolCeiling", ev.Error)
			}
			if got := ev.Error.Error(); !strings.Contains(got, "wedged-tool") {
				t.Fatalf("error %q should name the offending tool", got)
			}
			return
		case <-deadline:
			t.Fatal("the ceiling never failed the turn")
		}
	}
}

// A negative ceiling disables it: a tool may then run indefinitely, which is the
// escape hatch for an operator who would rather never lose a turn.
func TestHeartbeatCeilingDisabled(t *testing.T) {
	clk := &fakeClock{t: time.Unix(0, 0)}
	s, sub := newHeartbeatSession(t, Options{
		ToolHeartbeatInterval: time.Millisecond,
		ToolCeiling:           -1,
		Now:                   clk.now,
	})
	sub.watcher.Observe("t1", "long-tool", agent.TaskStatusInProgress)
	clk.advance(100 * time.Hour)

	go s.heartbeat(sub)
	defer sub.stopHeartbeat()

	deadline := time.After(500 * time.Millisecond)
	for {
		select {
		case ev := <-sub.events:
			if ev.Type == agent.EventError {
				t.Fatalf("a disabled ceiling still failed the turn: %v", ev.Error)
			}
			return // a keep-alive is the expected outcome
		case <-deadline:
			t.Fatal("expected keep-alives with the ceiling disabled")
		}
	}
}

// stopHeartbeat must be safe to call twice — completeActive and failActive can
// both reach it — and must return rather than block the second time.
func TestStopHeartbeatIsIdempotent(t *testing.T) {
	s, sub := newHeartbeatSession(t, Options{ToolHeartbeatInterval: time.Millisecond})
	go s.heartbeat(sub)

	done := make(chan struct{})
	go func() {
		sub.stopHeartbeat()
		sub.stopHeartbeat()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a second stopHeartbeat blocked")
	}
}
