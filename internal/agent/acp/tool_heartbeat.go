package acp

import (
	"context"
	"fmt"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

// The watcher, the ceiling error and both defaults live in internal/agent: the
// claudecode backend needs the identical behaviour, and the Slack relay matches
// the ceiling error with errors.Is, so a per-backend copy would silently diverge
// in exactly the way the user would notice.
const (
	defaultACPToolHeartbeatInterval = agent.DefaultToolHeartbeatInterval
	// A negative ProcessOptions.ToolCeiling disables the ceiling; zero takes this.
	defaultACPToolCeiling = agent.DefaultToolCeiling
)

// ErrToolCeiling is an alias for agent.ErrToolCeiling, kept so this package's own
// callers (prompt.go, the tests) read naturally. It is the same value, so an
// errors.Is match against either name matches both.
var ErrToolCeiling = agent.ErrToolCeiling

// heartbeat keeps a turn alive while a tool legitimately runs, and fails it when a
// tool runs too long. It ticks on interval; on each tick, if a tool is in flight it
// either emits a meta status event (resetting the gateway's idle watchdog, rendered
// as nothing) or, once the longest-running tool passes the ceiling, cancels the
// turn with ErrToolCeiling. When no tool is in flight it emits nothing, so a
// genuinely idle turn (e.g. a wedged provider call with no tool running) still
// trips the idle watchdog exactly as before — the ceiling only governs tools.
func (c *acpSession) heartbeat(ctx context.Context, w *agent.ToolWatcher, events chan<- agent.Event, cancel context.CancelCauseFunc, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	interval := c.opts.ToolHeartbeatInterval
	if interval <= 0 {
		interval = defaultACPToolHeartbeatInterval
	}
	ceiling := c.opts.ToolCeiling
	if ceiling == 0 {
		ceiling = defaultACPToolCeiling
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			active, title, oldest := w.Snapshot()
			if active == 0 {
				continue
			}
			if ceiling > 0 && oldest >= ceiling {
				if title == "" {
					title = "a tool"
				}
				c.log.Warn("ACP tool exceeded its ceiling; aborting turn", "tool", title, "elapsed", oldest.Round(time.Second), "ceiling", ceiling)
				cancel(fmt.Errorf("%w: %q ran for %s with no result", ErrToolCeiling, title, oldest.Round(time.Second)))
				return
			}
			select {
			case events <- agent.Event{Type: agent.EventStatus, Text: "still working…"}:
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}
}
