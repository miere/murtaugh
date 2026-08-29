// Package election drives a config.Locker through the leader lifecycle: contend
// for the lock, hold it while renewing, and stand down the moment the claim is
// no longer provably ours.
//
// The package deliberately knows nothing about Slack. Its job is to answer one
// question — "may this node act as the leader right now?" — correctly and
// cheaply, and to raise promote/demote callbacks when the answer changes. What
// the caller does with that answer is the caller's business.
//
// # Why the answer is not just a boolean
//
// The dangerous state in a leader election is not "no leader"; it is two
// leaders. Two Murtaugh gateways both connected to one Slack app receive every
// event and answer twice, and no amount of downstream care fixes it because
// Slack's API will accept writes from both.
//
// So the interesting logic is all on the demotion side, and it splits in two:
//
//   - The renewal loop demotes proactively, one renewal interval BEFORE the
//     lease would lapse, so an outgoing leader is already silent by the time a
//     challenger is entitled to promote.
//   - Allow re-verifies against the store when local timekeeping cannot be
//     trusted, which is the only defence against a process that was suspended
//     and woke up still believing it leads.
package election

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

// Callbacks are the transitions a caller reacts to. Both are invoked
// synchronously from the runner's goroutine, so a slow callback delays the next
// renewal — a promote handler that must do real work should hand off.
type Callbacks struct {
	// OnPromote is called once when this node takes leadership. An error is
	// logged and the leadership is given up, on the theory that a node that
	// cannot start serving should not hold the lock other nodes are waiting for.
	OnPromote func(ctx context.Context, lease config.Lease) error
	// OnDemote is called once when this node stops leading, for any reason:
	// lost lease, failed renewals, or shutdown. It must not block on anything
	// that could itself require leadership.
	OnDemote func(ctx context.Context, reason string)
}

// Runner owns one node's participation in the election.
type Runner struct {
	locker config.Locker
	cb     Callbacks
	logger *slog.Logger
	clock  Clock

	// renew is how often a held lease is refreshed; demoteAfter is how long
	// without a CONFIRMED renewal this node may keep acting. demoteAfter is
	// strictly less than the lease, so we stand down before a challenger may
	// stand up.
	renew       time.Duration
	demoteAfter time.Duration
	// retry is how long a standby waits between acquisition attempts.
	retry time.Duration

	mu sync.Mutex
	// lease is the currently held claim; zero when not leading.
	lease config.Lease
	// confirmed is when the lease was last proven ours, on both clocks.
	confirmed instant
	// leading mirrors lease.Held() but is kept separate so the promote/demote
	// callbacks fire exactly once per transition.
	leading bool
}

// Options configure a Runner. Only Locker is required.
type Options struct {
	Locker    config.Locker
	Callbacks Callbacks
	Logger    *slog.Logger
	// Fallback supplies the timings. Its lease value is ignored for a locker
	// that reports no TTL, since such a lock does not expire.
	Fallback config.FallbackConfig
	// Clock defaults to SystemClock. Tests substitute it to drive the
	// wall-versus-monotonic divergence that a real suspension produces.
	Clock Clock
}

// New builds a Runner.
func New(opts Options) (*Runner, error) {
	if opts.Locker == nil {
		return nil, errors.New("election: a locker is required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clock := opts.Clock
	if clock == nil {
		clock = SystemClock()
	}

	r := &Runner{
		locker:      opts.Locker,
		cb:          opts.Callbacks,
		logger:      logger,
		clock:       clock,
		renew:       opts.Fallback.EffectiveRenew(),
		demoteAfter: opts.Fallback.EffectiveDemoteAfter(),
		retry:       opts.Fallback.EffectiveRenew(),
	}

	// A locker with no TTL detects holder death itself — the kernel drops a
	// file lock when the process dies — so there is no lease to renew and
	// nothing to stand down from on a timer. Disabling the deadline here is
	// what lets one loop serve both backends.
	if opts.Locker.TTL() == 0 {
		r.demoteAfter = 0
	}
	return r, nil
}

// Run contends for leadership until ctx is cancelled.
//
// It never returns an error for losing an election: not being the leader is the
// expected state for every node but one. It returns only when ctx ends, having
// released the lock if it held it.
func (r *Runner) Run(ctx context.Context) error {
	defer r.standDown(context.WithoutCancel(ctx), "shutting down")

	for {
		wait := r.step(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// step performs one tick of the lifecycle and returns how long to wait before
// the next. Splitting it out keeps the loop trivial and makes each transition
// individually testable without racing a real timer.
func (r *Runner) step(ctx context.Context) time.Duration {
	if r.Leading() {
		r.renewOnce(ctx)
		return r.renew
	}
	r.acquireOnce(ctx)
	if r.Leading() {
		return r.renew
	}
	return r.retry
}

// acquireOnce makes one attempt to take the lock.
func (r *Runner) acquireOnce(ctx context.Context) {
	lease, ok, err := r.locker.Acquire(ctx)
	if err != nil {
		// Not knowing whether the lock is free is not the same as it being
		// free. Stay a standby and try again.
		r.logger.Warn("could not determine leader lock state", "error", err)
		return
	}
	if !ok {
		return
	}

	r.mu.Lock()
	r.lease = lease
	r.confirmed = now(r.clock)
	r.leading = true
	r.mu.Unlock()

	r.logger.Info("promoted to leader", "epoch", lease.Epoch, "owner", lease.Owner, "backend", r.locker.Backend())

	if r.cb.OnPromote != nil {
		if err := r.cb.OnPromote(ctx, lease); err != nil {
			// A node that cannot start serving must not sit on the lock that
			// another node is waiting for.
			r.logger.Error("promotion failed; giving up leadership", "error", err)
			r.standDown(ctx, "promotion failed: "+err.Error())
		}
	}
}

// renewOnce refreshes a held lease and stands down when the claim is lost or
// has gone unconfirmed for too long.
func (r *Runner) renewOnce(ctx context.Context) {
	r.mu.Lock()
	lease := r.lease
	r.mu.Unlock()

	renewed, ok, err := r.locker.Renew(ctx, lease)
	switch {
	case err != nil:
		// A transport failure means "cannot tell", not "lost". Keep the lease
		// and let the unconfirmed deadline below decide when holding on stops
		// being defensible.
		r.logger.Warn("leader lease renewal failed", "error", err)
	case !ok:
		// An unambiguous loss: another node holds the lock. Stand down at once
		// rather than waiting for a deadline.
		r.standDown(ctx, "the leader lock was taken by another node")
		return
	default:
		r.mu.Lock()
		r.lease = renewed
		r.confirmed = now(r.clock)
		r.mu.Unlock()
		return
	}

	if r.unconfirmedTooLong() {
		r.standDown(ctx, "the leader lease could not be renewed in time")
	}
}

// unconfirmedTooLong reports whether too much time has passed since the lease
// was last proven ours.
//
// It takes the LARGER of the two elapsed measurements. Monotonic time is the
// right measure for ordinary operation, but it does not advance while the
// machine is suspended, so on its own it would report a four-hour sleep as a
// few milliseconds and keep a long-dead leader believing it still leads. Wall
// time catches that, at the cost of being fooled by an NTP step — and being
// fooled towards standing down is the safe direction to be wrong in.
func (r *Runner) unconfirmedTooLong() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.demoteAfter <= 0 || !r.leading {
		return false
	}
	wall, mono := r.confirmed.since(now(r.clock))
	return max(wall, mono) >= r.demoteAfter
}

// Leading reports whether this node currently believes it is the leader.
//
// It is a cheap cached read, suitable for a hot path that merely wants to skip
// work. It is NOT sufficient to authorize an externally visible side effect:
// use Allow for that.
func (r *Runner) Leading() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.leading
}

// Lease returns the currently held lease, or the zero value when not leading.
func (r *Runner) Lease() config.Lease {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lease
}

// Allow reports whether this node may perform an externally visible action as
// the leader right now. It is the gate every outbound Slack write must pass.
//
// Most calls are answered from cached state, because a lease confirmed a second
// ago is still good. The call goes to the store in exactly two situations, both
// of which mean local timekeeping cannot be trusted to answer:
//
//   - The lease has not been confirmed within a renewal interval, so a renewal
//     has been missed and the claim may already be gone.
//   - Wall-clock time has advanced far more than monotonic time, which means
//     this process was not running for the difference. A suspended laptop is
//     the ordinary cause, and it is precisely the case a purely local check
//     cannot detect: the monotonic clock it would consult was suspended too.
//
// The cost is one read on a path that is already making a network call to
// Slack, and it is the only thing standing between a resumed laptop and a
// second gateway answering every message.
func (r *Runner) Allow(ctx context.Context) bool {
	r.mu.Lock()
	if !r.leading {
		r.mu.Unlock()
		return false
	}
	lease := r.lease
	wall, mono := r.confirmed.since(now(r.clock))
	r.mu.Unlock()

	suspended := wall-mono > suspendSlack
	if !suspended && r.demoteAfter > 0 && wall < r.renew && mono < r.renew {
		return true
	}
	// A locker with no lease cannot go stale on a timer, but it CAN be lost by
	// other means (its file replaced, its process's lock dropped), so a
	// suspension still warrants a real check rather than a cached yes.
	if !suspended && r.demoteAfter == 0 {
		return true
	}

	ok, err := r.locker.Verify(ctx, lease)
	if err != nil {
		// Unable to prove leadership is not permission to act as leader. This
		// is the fail-closed direction, and it is the correct one: a false
		// negative costs a dropped reply, a false positive costs a duplicate
		// gateway.
		r.logger.Warn("could not verify leadership; withholding leader action", "error", err, "suspended", suspended)
		return false
	}
	if !ok {
		r.standDown(ctx, r.lostReason(suspended))
		return false
	}

	r.mu.Lock()
	r.confirmed = now(r.clock)
	r.mu.Unlock()
	return true
}

// lostReason describes why leadership was found to be gone, so the operator
// reading a demotion notice learns something useful rather than just "lost".
func (r *Runner) lostReason(suspended bool) string {
	if suspended {
		return "this node was suspended and lost the leader lock while it was not running"
	}
	return "the leader lock is no longer held by this node"
}

// standDown relinquishes leadership exactly once per promotion, fires OnDemote,
// and releases the lock so a standby can promote without waiting out the lease.
//
// The demote callback runs BEFORE the release. Handing the lock over while this
// node is still capable of acting would open the very overlap the whole design
// exists to close.
func (r *Runner) standDown(ctx context.Context, reason string) {
	r.mu.Lock()
	if !r.leading {
		r.mu.Unlock()
		return
	}
	lease := r.lease
	r.leading = false
	r.lease = config.Lease{}
	r.confirmed = instant{}
	r.mu.Unlock()

	r.logger.Warn("standing down as leader", "reason", reason, "epoch", lease.Epoch)

	if r.cb.OnDemote != nil {
		r.cb.OnDemote(ctx, reason)
	}
	if err := r.locker.Release(ctx, lease); err != nil {
		// Failing to release is survivable: the lease expires on its own, so the
		// cost is a slower handover rather than a stuck cluster.
		r.logger.Warn("could not release the leader lock; a standby will wait for the lease to lapse", "error", err)
	}
}
