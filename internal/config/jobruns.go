package config

import (
	"context"
	"errors"
	"strings"
	"time"
)

// This file defines the scheduled-run claim seam. It is the scheduler's answer
// to the same problem leader election solves for the socket: with several nodes
// taking turns, "who runs this job" needs an arbiter that outlives any one of
// them.
//
// Election alone is not that arbiter. It guarantees one leader at a time, which
// stops two nodes firing a job simultaneously — but the scheduler's clock is
// per-process, so leadership changing hands still perturbs it:
//
//   - A leader that restarts within a job's window rebuilds its scheduler from
//     scratch. An interval job that fired five minutes ago starts a fresh
//     interval and fires again well before it should.
//   - The same is true of a promotion: the incoming leader has no idea what the
//     outgoing one already ran.
//
// A claim recorded in the SHARED store fixes both, because it is the one piece
// of scheduling state that survives the process that made it.

// JobRunClaim is a scheduled job's occurrence: the job's name plus the slot it
// is firing for. Two nodes computing the slot for the same moment must agree,
// which is why it is derived from wall-clock truncation against a fixed origin
// rather than from either node's uptime.
type JobRunClaim struct {
	// Job is the configured job name.
	Job string
	// Occurrence identifies the slot. It is UTC and truncated to the job's
	// resolution, so the same fire produces the same value on every node.
	Occurrence time.Time
}

// Validate reports whether the claim is usable as a store key.
func (c JobRunClaim) Validate() error {
	if strings.TrimSpace(c.Job) == "" {
		return errors.New("job run claim: job name is required")
	}
	if c.Occurrence.IsZero() {
		return errors.New("job run claim: occurrence is required")
	}
	return nil
}

// JobRunStore records which scheduled occurrences have been claimed.
//
// It lives in the same store as the configuration and the leader lock, for the
// same reason: nodes serving one Slack app must agree on where this state is,
// and the store their shared config came from is the only place they already
// agree on.
type JobRunStore interface {
	// Claim atomically records the occurrence and reports whether THIS caller
	// took it. A false with a nil error means someone already has it — an
	// ordinary outcome, not a failure.
	//
	// It is the whole of the concurrency contract: callers run the job if and
	// only if Claim returned true.
	Claim(ctx context.Context, claim JobRunClaim, node string) (bool, error)

	// LastRun returns the most recent claimed occurrence for a job, for
	// diagnostics and for reporting what a node skipped.
	LastRun(ctx context.Context, job string) (time.Time, bool, error)

	// Prune deletes claims older than before, so a job firing every minute does
	// not accumulate rows forever.
	Prune(ctx context.Context, before time.Time) (int64, error)

	// Close releases the backend handle.
	Close() error
}

// JobRunRetention is how long claims are kept.
//
// They are only ever compared against the immediate present — "has this slot
// already been taken" — so the history has no operational use beyond a few
// days' worth of "did last night's backup run". A week is generous for that and
// keeps a minutely job's table in the low thousands of rows.
const JobRunRetention = 7 * 24 * time.Hour

// OccurrenceFor derives the claim slot for a fire at `at` with the given
// resolution.
//
// Truncation is against the Unix epoch, not against any node's start time, so
// two nodes that fire the same job moments apart compute the SAME slot and
// exactly one wins the claim. Deriving it from uptime — the obvious thing, since
// that is what the scheduler itself counts — would give every node its own
// grid, and every node would win its own claim.
func OccurrenceFor(at time.Time, resolution time.Duration) time.Time {
	if resolution <= 0 {
		resolution = time.Minute
	}
	return at.UTC().Truncate(resolution)
}

// ScheduleResolution is the slot size for a job's schedule kind.
//
// Cron resolves to the minute because that is cron's own granularity: a job
// due at 09:00 fires once in the 09:00 minute, whichever node is leading.
// Interval jobs resolve to their own period, which is what makes a restart mid
// period a no-op rather than a second run.
func (p JobProfile) ScheduleResolution() time.Duration {
	switch p.ScheduleKind() {
	case ScheduleCron:
		return time.Minute
	case ScheduleEvery:
		if d, err := time.ParseDuration(strings.TrimSpace(p.Every)); err == nil && d > 0 {
			return d
		}
		return time.Minute
	default:
		return 0
	}
}
