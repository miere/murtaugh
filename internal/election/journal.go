package election

import (
	"context"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/journal"
)

// This file records the election to the journal.
//
// It exists because a failover leaves almost no trace otherwise. The one thing
// an operator wants after the bot went quiet is the sequence — who led, when
// they stood down, and what the store said at the time — and until now that
// lived only in whichever node's launchd log happened to still exist. A node
// that loses its credentials mid-flight is the sharp case: it stands down
// correctly and silently, and the new leader knows a lease lapsed but not why.
//
// Events go to the gateway stream alongside the connection events, because an
// operator debugging "Murtaugh stopped answering" is already reading that
// stream and should not have to know that leadership is a separate concern.

// electionKind is the event kind for every election event, so one filter
// (`journal query --stream gateway --kind election`) reconstructs a failover.
const electionKind = "election"

// record emits one election event. A nil recorder is a no-op, which is what
// CLI/MCP builds and most tests have.
func (r *Runner) record(ctx context.Context, level journal.Level, state, summary string, extra map[string]any) {
	if r.recorder == nil {
		return
	}
	payload := map[string]any{
		"state":   state,
		"backend": r.locker.Backend(),
	}
	for k, v := range extra {
		payload[k] = v
	}
	r.recorder.Record(ctx, journal.Event{
		Stream:  journal.StreamGateway,
		Kind:    electionKind,
		Level:   level,
		Summary: summary,
		Payload: payload,
	})
}

// leaseFields adds the lease's identity to an event. The epoch is what makes a
// sequence of events across several nodes orderable after the fact: two nodes'
// logs interleave arbitrarily, but their epochs do not.
func leaseFields(lease config.Lease) map[string]any {
	return map[string]any{"epoch": lease.Epoch, "owner": lease.Owner}
}
