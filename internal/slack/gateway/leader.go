package gateway

import (
	"context"
	"fmt"
	"time"
)

// LeaderElector is the gateway's view of leader election. It is deliberately
// narrow — two methods — so this package does not depend on the election
// implementation, matching how RestartTrigger keeps the restart coordinator out
// of here.
//
// The elector's own promote/demote callbacks are what call StartServing and
// StopServing; the composition root wires them, because it is the only place
// that knows about both halves.
type LeaderElector interface {
	// Run drives the election until ctx is cancelled.
	Run(ctx context.Context) error
	// Allow reports whether this node may perform an externally visible leader
	// action right now. It backs the outbound gate, so it is called on every
	// Slack request and must be cheap in the common case.
	Allow(ctx context.Context) bool
}

// drainTimeout bounds how long demotion waits for in-flight agent turns to
// finish before cancelling them.
//
// The wait exists because killing a turn mid-write leaves a half-edited file
// and, for a claude_code agent, possibly an orphaned git worktree — the node is
// losing Slack, and there is no reason for it to lose the workspace too. The
// bound exists because a standby is meanwhile waiting to take over, and a turn
// that has not finished in this long is not about to.
const drainTimeout = 60 * time.Second

// drainPoll is how often the drain checks whether the last turn has finished.
const drainPoll = 250 * time.Millisecond

// effectiveDrainTimeout is how long demotion waits for in-flight turns. Tests
// shorten it; production leaves it at drainTimeout.
func (a *Gateway) effectiveDrainTimeout() time.Duration {
	if a.drainWait > 0 {
		return a.drainWait
	}
	return drainTimeout
}

// WithDrainTimeout overrides how long a demotion waits for in-flight agent
// turns before cancelling them.
func (a *Gateway) WithDrainTimeout(d time.Duration) *Gateway {
	a.drainWait = d
	return a
}

// WithLeaderElection makes this gateway contend for leadership rather than
// serving unconditionally. nil leaves the single-node behaviour untouched.
func (a *Gateway) WithLeaderElection(elector LeaderElector) *Gateway {
	a.electorMu.Lock()
	a.elector = elector
	a.electorMu.Unlock()
	return a
}

// leaderAllows backs the outbound gate. With no elector wired every call is
// permitted, so a single-node deployment pays nothing for a feature it did not
// turn on.
func (a *Gateway) leaderAllows(ctx context.Context) bool {
	a.electorMu.RLock()
	elector := a.elector
	a.electorMu.RUnlock()
	if elector == nil {
		return true
	}
	return elector.Allow(ctx)
}

// StartServing brings the gateway up as leader. It is the elector's promote
// callback.
//
// The allowlist resolution runs synchronously and its failure is returned,
// which makes promotion itself fail. That matters: it is a real Slack round
// trip, so a node that cannot reach Slack discovers it here and gives the lock
// back, rather than holding it while unable to serve — which would convert one
// node's problem into a total outage.
func (a *Gateway) StartServing(ctx context.Context) error {
	a.serveMu.Lock()
	if a.serveCancel != nil {
		a.serveMu.Unlock()
		return nil // already serving; promotion is idempotent
	}
	a.serveMu.Unlock()

	resolveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := a.resolveAllowSet(resolveCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("resolve allowed users: %w", err)
	}
	if a.auth != nil {
		a.auth.SetAdmin(a.access().AdminUser, a.access().IsAdminUser)
	}

	// Child of the caller's context so daemon shutdown stops serving too; the
	// stored cancel is what demotion uses to stop it without shutting down.
	serveCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})

	a.serveMu.Lock()
	a.serveCancel = stop
	a.serveDone = done
	a.serveMu.Unlock()

	// Reset the connect-time greeting BEFORE the socket goroutine starts, so
	// the write happens-before that goroutine's reads. A node taking over after
	// a failover has a genuinely new connection to announce, and connectHandled
	// is per-connection; StopServing waits for the previous goroutine to exit,
	// so successive promotions never overlap on this field.
	a.connectHandled = false

	go func() {
		defer close(done)
		a.startBridge(serveCtx)
		a.logStartupRouting(serveCtx)
		a.warmChat(serveCtx)
		a.startChannelCache(serveCtx)
		a.startJournalSweeper(serveCtx)
		stopScheduler := a.startScheduler(serveCtx)
		defer stopScheduler()

		if err := a.superviseSocket(serveCtx); err != nil {
			a.logger.Error("Slack socket supervisor stopped", "error", err)
		}
	}()
	return nil
}

// StopServing stands the gateway down. It is the elector's demote callback.
//
// The order is the whole of the safety argument, and it is not the intuitive
// one:
//
//  1. The outbound gate has ALREADY closed, before this runs — the elector
//     clears its leadership flag first, so by the time we are here no further
//     Slack write can leave this process. Nothing below can leak a message.
//  2. Cancel the serve context: the socket disconnects and the scheduler stops,
//     so no new work arrives and no cron fires.
//  3. Drain the turns already running, then cancel whatever is left.
//
// Draining last rather than first is deliberate. The danger of a demoted node
// was never that its work continued; it was that its work replied. With the
// gate shut, a turn that keeps running for another few seconds harms nothing
// and finishes writing the file it was halfway through.
func (a *Gateway) StopServing(ctx context.Context, reason string) {
	a.serveMu.Lock()
	stop, done := a.serveCancel, a.serveDone
	a.serveCancel, a.serveDone = nil, nil
	a.serveMu.Unlock()

	if stop == nil {
		return // never promoted, or already stood down
	}

	a.logger.Warn("standing down: disconnecting from Slack and stopping scheduled jobs", "reason", reason)
	stop()

	// Wait for the socket supervisor and scheduler to unwind. Bounded because a
	// half-open socket's RunContext may never return — the supervisor already
	// abandons it in that case, and so do we.
	select {
	case <-done:
	case <-time.After(a.effectiveDrainTimeout()):
		a.logger.Warn("the Slack socket supervisor did not stop in time; continuing to stand down")
	}

	a.drainInFlight(ctx)
}

// drainInFlight waits for running agent turns to finish, then cancels any that
// outlast the deadline.
func (a *Gateway) drainInFlight(ctx context.Context) {
	if a.inFlight == nil {
		return
	}
	wait := a.effectiveDrainTimeout()
	if remaining := a.inFlight.Len(); remaining > 0 {
		a.logger.Info("draining in-flight agent turns before standing down", "turns", remaining, "timeout", wait)
	}

	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	ticker := time.NewTicker(min(drainPoll, wait))
	defer ticker.Stop()

	for a.inFlight.Len() > 0 {
		select {
		case <-ctx.Done():
			// The daemon is shutting down, so there is no successor to wait for
			// and no point finishing work nobody will read.
			a.cancelStragglers("shutdown interrupted the drain")
			return
		case <-deadline.C:
			a.cancelStragglers("they outlasted the drain timeout")
			return
		case <-ticker.C:
		}
	}
	a.logger.Info("all in-flight agent turns finished; stood down cleanly")
}

// cancelStragglers stops the turns that did not finish within the drain window.
func (a *Gateway) cancelStragglers(why string) {
	if cancelled := a.inFlight.CancelAll(); cancelled > 0 {
		a.logger.Warn("cancelled in-flight agent turns", "turns", cancelled, "reason", why)
	}
}
