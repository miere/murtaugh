package gateway

import (
	"context"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

// RunClaimer decides whether this node may execute a scheduled job's
// occurrence. It returns false when the slot is already taken — by another
// node, or by an earlier incarnation of this one.
//
// The gateway takes a function rather than the store interface for the same
// reason it takes ScheduledRunner and JobConfirmer as closures: the composition
// root owns the store, and this package stays free of any dependency on it.
type RunClaimer func(ctx context.Context, claim config.JobRunClaim) (bool, error)

// claimTimeout bounds the claim. It is deliberately short: the claim gates a
// job that is due NOW, so waiting a long time to find out whether we may run it
// defeats the schedule as surely as not running it.
const claimTimeout = 10 * time.Second

// WithRunClaimer wires the shared scheduled-run claim store.
//
// nil leaves the scheduler unguarded, which is correct for CLI/MCP and tests —
// they have no scheduler — and is also what a struct-literal gateway gets.
func (a *Gateway) WithRunClaimer(claim RunClaimer) *Gateway {
	a.claimRun = claim
	return a
}

// claimScheduledRun reports whether this node should execute name right now.
//
// # Why a claim rather than trusting the scheduler
//
// gocron counts from when its scheduler started, which is per-process. That is
// fine for a daemon that runs forever and wrong for one that hands leadership
// around:
//
//   - A leader that restarts mid-interval rebuilds its scheduler from zero. An
//     `every: 1h` job that ran five minutes ago starts a fresh hour and fires
//     again far too soon.
//   - A promoted standby is in the same position, with the added problem that
//     it never saw what its predecessor ran at all.
//
// The occurrence slot is computed by truncating wall-clock time against a fixed
// origin, so every node computes the SAME slot for the same moment and exactly
// one insert wins. Deriving it from process uptime — the obvious choice, since
// that is what the scheduler itself counts — would give each node its own grid
// and let every node win its own claim.
//
// # Why a failed claim does not run
//
// An unreachable store means we cannot tell whether the slot is taken. Not
// running is the safe reading: a skipped occurrence is visible in the log and
// recovers on the next tick, whereas a duplicate run of a job that moves money,
// sends mail, or rewrites a repository may not recover at all.
func (a *Gateway) claimScheduledRun(ctx context.Context, name string) bool {
	if a.claimRun == nil {
		return true
	}
	job, ok := a.scheduledJobs[name]
	if !ok {
		return true
	}
	resolution := job.ScheduleResolution()
	if resolution <= 0 {
		return true // manual job: no slot to contend for
	}

	claim := config.JobRunClaim{
		Job:        name,
		Occurrence: config.OccurrenceFor(a.clock(), resolution),
	}

	claimCtx, cancel := context.WithTimeout(ctx, claimTimeout)
	defer cancel()

	claimed, err := a.claimRun(claimCtx, claim)
	if err != nil {
		a.logger.Error("skipping scheduled job: could not claim its run",
			"job", name, "occurrence", claim.Occurrence.Format(time.RFC3339), "error", err)
		return false
	}
	if !claimed {
		// Info, not warn: this is the mechanism working. It fires on every
		// restart that lands inside a job's window, which is normal operation
		// rather than a fault.
		a.logger.Info("skipping scheduled job: this run was already claimed",
			"job", name, "occurrence", claim.Occurrence.Format(time.RFC3339))
		return false
	}
	return true
}
