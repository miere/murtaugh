// heartbeat.go keeps a claudecode turn alive while a tool legitimately runs.
//
// The gateway's idle watchdog resets on any agent.Event, so a turn that produces
// no events for the idle window is treated as stalled and abandoned. That is the
// right default — an agent which has genuinely gone quiet should not hold a
// thread open — but it cannot tell "wedged" apart from "waiting", and the
// claudecode backend has several legitimate waits that emit nothing at all:
//
//   - an MCP tool blocking on a human (the auth card and the ask card both wait
//     up to ten minutes, which is the idle default exactly)
//   - a can_use_tool approval sitting on an unread Slack card
//   - any slow tool whose output arrives only at the end
//
// Between the `tool_use` block that starts a tool and the `tool_result` that
// retires it, the stream is silent. The native loop and the ACP client each
// already solve this; this file is the third backend catching up, on the shared
// agent.ToolWatcher so all three stay in step.
package claudecode

import (
	"fmt"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

// heartbeatText is what a keep-alive carries. EventStatus is a meta event the
// renderer does not print, so the text is for logs and tests rather than the
// user; it matches the ACP wording so a journal reads the same across backends.
const heartbeatText = "still working…"

// heartbeat ticks for the life of one turn. On each tick, if a tool is in flight
// it either emits a keep-alive (resetting the gateway's idle watchdog) or, once
// the longest-running tool passes the ceiling, fails the turn.
//
// When no tool is in flight it emits nothing. That is deliberate: a turn idle for
// some other reason — a wedged provider call, a process that stopped talking —
// must still trip the watchdog exactly as before. The ceiling governs tools only.
func (s *procSession) heartbeat(sub *subscription) {
	defer close(sub.hbDone)

	interval := s.opts.ToolHeartbeatInterval
	if interval <= 0 {
		interval = agent.DefaultToolHeartbeatInterval
	}
	ceiling := s.opts.ToolCeiling
	if ceiling == 0 {
		ceiling = agent.DefaultToolCeiling
	}

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-sub.hbStop:
			return
		case <-t.C:
			active, title, oldest := sub.watcher.Snapshot()
			if active == 0 {
				continue
			}
			if ceiling > 0 && oldest >= ceiling {
				if title == "" {
					title = "a tool"
				}
				s.log.Warn("claudecode: tool exceeded its ceiling; failing turn",
					"tool", title, "elapsed", oldest.Round(time.Second), "ceiling", ceiling)
				// Fail from a separate goroutine. failActive stops this heartbeat
				// and waits for hbDone before closing the event channel, so calling
				// it inline would block on our own exit signal forever.
				go s.failActive(fmt.Errorf("%w: %q ran for %s with no result",
					agent.ErrToolCeiling, title, oldest.Round(time.Second)))
				return
			}
			select {
			case sub.events <- agent.Event{Type: agent.EventStatus, Text: heartbeatText}:
			case <-sub.hbStop:
				return
			}
		}
	}
}

// stopHeartbeat signals the turn's heartbeat and waits for it to exit.
//
// The wait is the point: the heartbeat writes to sub.events, and the caller is
// about to close that channel. Without it a tick landing between "turn ended" and
// "channel closed" would panic the gateway on a send to a closed channel. sync.Once
// makes it safe for completeActive and failActive to both call it.
func (sub *subscription) stopHeartbeat() {
	if sub.hbStop == nil {
		return
	}
	sub.hbOnce.Do(func() { close(sub.hbStop) })
	<-sub.hbDone
}
