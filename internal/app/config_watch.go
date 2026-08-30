package app

import (
	"context"
	"time"

	"github.com/miere/murtaugh/internal/config"
	gateway "github.com/miere/murtaugh/internal/slack/gateway"
)

// This file implements the hot-reload policy: the running configuration is
// watched, and any change to it is shown to the admin as a diff and adopted
// only if they approve.
//
// The alternative it replaces — load once at startup — stopped being tenable
// when leader election arrived. A standby can sit for a week before it is
// promoted, so "the configuration this process booted with" and "the
// configuration the operator believes is live" drift arbitrarily far apart, and
// the moment that matters is exactly the moment nobody is watching.

// configPollInterval is how often the store is re-read.
//
// Polling rather than subscribing because none of the three backends offers a
// change feed the others do, and a poll is uniform across all of them. The
// interval is a compromise between noticing an edit promptly and not making a
// query every second forever; a config change is a human-scale event, so half a
// minute is well inside "did it not see my edit?".
const configPollInterval = 30 * time.Second

// configApprovalTimeout bounds how long a pending decision stays open.
//
// It exists so a change made and then ignored does not pin the watcher open
// indefinitely. Expiry means "not approved": the running configuration is
// restored, exactly as an explicit rollback would.
const configApprovalTimeout = 30 * time.Minute

// watchConfig polls the store and drives the approval flow.
//
// It runs only while this node leads. A standby has no business prompting: it
// is not serving, its outbound gate is shut so the card could not be posted
// anyway, and two nodes asking the admin about one edit would be worse than
// either asking alone.
func (a *Application) watchConfig(ctx context.Context, holder *gatewayHolder, runner leaderRunner) {
	if a.cfgStore == nil {
		return
	}
	approved, err := a.cfgStore.Snapshot(ctx)
	if err != nil {
		a.logger.Warn("configuration watch disabled: could not read the running configuration", "error", err)
		return
	}

	ticker := time.NewTicker(configPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		// A configuration Murtaugh wrote itself (the agent setup form) hands
		// over a new baseline rather than being reviewed: asking somebody to
		// approve a change they made through a form thirty seconds ago is how a
		// review prompt becomes noise.
		if adopted, ok := a.takeApprovedConfig(); ok {
			approved = adopted
		}
		if !runner.Leading() {
			continue
		}
		next, ok := a.reviewConfigChange(ctx, holder, runner, approved)
		if ok {
			approved = next
		}
	}
}

// reviewConfigChange performs one poll. It returns the snapshot that is now
// authoritative, and whether it changed.
//
// Whichever way the admin decides, the store and this node end up agreeing:
// approval reloads onto the new snapshot, rejection writes the old one back.
// That is what makes the next poll quiet rather than re-prompting forever.
func (a *Application) reviewConfigChange(ctx context.Context, holder *gatewayHolder, runner leaderRunner, approved config.Snapshot) (config.Snapshot, bool) {
	current, err := a.cfgStore.Snapshot(ctx)
	if err != nil {
		a.logger.Warn("could not read the configuration store", "error", err)
		return approved, false
	}
	changed, err := config.SnapshotChanged(approved, current)
	if err != nil {
		a.logger.Warn("could not compare configurations", "error", err)
		return approved, false
	}
	if !changed {
		return approved, false
	}

	diff, err := config.DiffSnapshots(approved, current, 3)
	if err != nil {
		a.logger.Warn("could not render the configuration diff", "error", err)
		return approved, false
	}
	a.logger.Info("configuration change detected; asking the admin to review")

	askCtx, cancel := context.WithTimeout(ctx, configApprovalTimeout)
	defer cancel()

	decision, err := holder.get().RequestConfigApproval(askCtx, diff)
	if err != nil {
		// Unreachable admin, no DM, expired request: none of these is approval.
		// Roll back so the store matches what is actually running, rather than
		// leaving an unreviewed edit sitting there to be adopted by whichever
		// node restarts next.
		a.logger.Warn("configuration change was not approved", "error", err)
		return a.rollbackConfig(ctx, approved), true
	}

	if decision == gateway.ConfigApply {
		newCfg, err := a.cfgStore.Load(ctx, a.cfg)
		if err != nil {
			// The approved edit does not assemble. Reverting is the only way
			// back to a daemon that can run at all.
			a.logger.Error("the approved configuration is invalid; rolling back", "error", err)
			return a.rollbackConfig(ctx, approved), true
		}
		// The watcher's ctx IS the daemon's, so it serves as both here.
		if err := a.reloadConfig(ctx, ctx, holder, runner, newCfg); err != nil {
			a.logger.Error("configuration reload failed", "error", err)
			return approved, false
		}
		return current, true
	}

	return a.rollbackConfig(ctx, approved), true
}

// rollbackConfig restores the running configuration over a rejected edit and
// returns the snapshot now in the store.
//
// A failed rollback is reported rather than retried in a loop: the store is
// then in a state neither side asked for, and the next poll will show the admin
// a diff again — which is noisy, but is at least visible, and is better than
// silently running one configuration while the store holds another.
func (a *Application) rollbackConfig(ctx context.Context, approved config.Snapshot) config.Snapshot {
	if err := config.RevertToSnapshot(ctx, a.cfgStore, approved); err != nil {
		a.logger.Error("could not roll the configuration back", "error", err)
		return approved
	}
	a.logger.Info("configuration rolled back to the running state")
	return approved
}

// startConfigWatch launches the watcher for the lifetime of ctx.
func (a *Application) startConfigWatch(ctx context.Context, holder *gatewayHolder, runner leaderRunner) {
	go a.watchConfig(ctx, holder, runner)
}

// compile-time assurance that the gateway satisfies what the watcher needs.
var _ interface {
	RequestConfigApproval(context.Context, string) (gateway.ConfigDecision, error)
} = (*gateway.Gateway)(nil)
