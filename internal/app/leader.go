package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/store"
	"github.com/miere/murtaugh/internal/election"
)

// wireLeaderElection builds the election runner that makes this node contend
// for leadership.
//
// It runs for every gateway, unconditionally. Election is not opt-in: the
// configuration backend always supplies a lock, and its kind is what varies —
// SQLite guarantees one gateway per machine, Postgres and Firestore one per
// cluster. A node that served without contending would be the duplicate
// gateway the whole mechanism exists to prevent.
//
// This is the only place that knows about both halves — the election package
// and the gateway — which is why the promote/demote callbacks are built here
// rather than in either of them.
//
// Every failure here is fatal rather than degraded, which is the opposite of
// how this composition root treats most optional collaborators. That is the
// point: the degraded state of leader election is a second gateway answering
// every message, so refusing to start is the safe failure and carrying on is
// not.
func (a *Application) wireLeaderElection(ctx context.Context, holder *gatewayHolder) (*election.Runner, error) {
	identity, err := resolveLockIdentity(ctx, a.cfg.OAuth.BotToken)
	if err != nil {
		return nil, fmt.Errorf("leader election: %w", err)
	}

	locker, err := store.OpenLocker(ctx, a.cfg.Database, identity, a.cfg.Election.EffectiveLease())
	if err != nil {
		if errors.Is(err, config.ErrLockUnsupported) {
			// Naming the backend matters: an operator who only hears
			// "unsupported" has to go looking for which knob that means.
			return nil, fmt.Errorf(
				"the %q config-store backend cannot provide a leader lock on this host, and Murtaugh will not run a gateway without one: %w",
				a.cfg.Database.EffectiveBackend(), err)
		}
		return nil, fmt.Errorf("leader election: open lock: %w", err)
	}

	runner, err := a.followElection(locker, holder)
	if err != nil {
		_ = locker.Close()
		return nil, fmt.Errorf("leader election: %w", err)
	}

	a.logger.Info("leader election active",
		"backend", locker.Backend(),
		"identity", identity.String(),
		"lease", a.cfg.Election.EffectiveLease(),
		"renew", a.cfg.Election.EffectiveRenew(),
		"stands_down_after", a.cfg.Election.EffectiveDemoteAfter())

	a.leaderLocker = locker
	return runner, nil
}

// followElection builds the runner over an already-open lock and hands it to
// the inbound node listener.
//
// It is separate from wireLeaderElection because everything above it needs a
// Slack workspace and a configuration store, and none of the three points where
// the listener meets the election does — so this is the seam the wiring test
// drives. The three are each a single line here and each of them was, until
// #197's review, deletable with the whole suite still green.
func (a *Application) followElection(locker config.Locker, holder *gatewayHolder) (*election.Runner, error) {
	runner, err := election.New(a.electionOptions(locker, holder))
	if err != nil {
		return nil, err
	}

	// The listener has been bound since process start; this is what decides
	// whether it ACCEPTS. Until it is called the endpoint refuses everything,
	// which is why it is wired here — the last thing before the election runs —
	// rather than at the listener's own construction, where no election exists
	// to consult.
	if a.nodeEndpoint.Follow != nil {
		a.nodeEndpoint.Follow(runner)
	}
	return runner, nil
}

// electionOptions is where the gateway, the inbound node listener and the
// election meet. Two of the three node-endpoint wirings live in it — the
// address a standby redirects to, and the drop on demotion — and the third is
// in followElection, which calls it.
func (a *Application) electionOptions(locker config.Locker, holder *gatewayHolder) election.Options {
	return election.Options{
		Locker:   locker,
		Election: a.cfg.Election,
		Logger:   a.logger.With("component", "election"),
		// Journalled to the gateway stream: an operator debugging "Murtaugh
		// stopped answering" is already reading that stream, and a failover
		// otherwise leaves its trace only in whichever node's launchd log
		// happens to still exist.
		Recorder: a.recorder,
		// Where this gateway accepts runtime nodes, written into the lock record
		// on promotion so a standby — already contending for the same lock —
		// can redirect a node to the leader instead of dropping it. nil for a
		// binary that opened no node listener, which is every gateway shipping
		// today, and reads as "the leader accepts no nodes".
		Address: a.nodeEndpoint.Address,
		Callbacks: election.Callbacks{
			// Both reach the CURRENT gateway through the holder rather than a
			// captured pointer: a configuration reload replaces it, and the
			// election must keep driving whichever one is live rather than a
			// torn-down predecessor.
			OnPromote: func(ctx context.Context, lease config.Lease) error {
				gw := holder.get()
				if err := gw.StartServing(ctx); err != nil {
					return err
				}
				gw.AnnouncePromotion(ctx, lease.Epoch, promotionReason(lease))
				// Promotion is the moment missed schedules become visible:
				// whatever the previous leader failed to run is this node's to
				// report. Off the critical path — a failed report must not fail
				// a promotion.
				go a.reportMissedJobs(context.WithoutCancel(ctx), gw)
				// A daemon with no agent is running correctly and answering
				// nobody, which from the operator's side looks exactly like a
				// broken one. Say so and offer the form.
				if !hasAgents(a.cfg) {
					go gw.NotifyNoAgents(context.WithoutCancel(ctx))
				}
				return nil
			},
			OnDemote: func(ctx context.Context, reason string) {
				holder.get().StopServing(ctx, reason)
				// After the Slack side has drained, and only then: a node
				// dropped first would redial into the new leader while this one
				// was still finishing a turn on its behalf. Nodes are dropped
				// rather than left attached because a node cannot tell that the
				// gateway it holds a socket to has stopped leading — it would
				// sit there, connected and unreachable, until somebody noticed.
				if a.nodeEndpoint.Detach != nil {
					a.nodeEndpoint.Detach(reason)
				}
			},
		},
	}
}

// promotionReason turns a lease into the sentence the takeover notice carries.
//
// The epoch tells us which case this is. Epoch 1 is the first leader this lock
// has ever had — a fresh deployment, nothing failed. Anything higher means this
// node took over from a predecessor, which is the case an operator wants to
// know about.
func promotionReason(lease config.Lease) string {
	if lease.Epoch <= 1 {
		return "first leader for this Slack app"
	}
	return "took over from the previous leader"
}

// slackIdentityTimeout bounds the auth.test call that resolves the lock key.
const slackIdentityTimeout = 15 * time.Second

// resolveLockIdentity asks Slack which workspace and app this token belongs to.
//
// The lock is keyed on team + app rather than on a hash of the token, because
// a token rotation would otherwise change the key: the incumbent would keep
// holding the lock under the old hash while a new node took a different lock
// under the new one, and both would connect to the same Slack app. Team and app
// survive rotation.
//
// Making this call before contending is safe. auth.test is a plain Web API
// read with no socket attached, so a standby that never becomes leader has done
// nothing but ask who it would have been.
func resolveLockIdentity(ctx context.Context, botToken string) (config.LockIdentity, error) {
	if strings.TrimSpace(botToken) == "" {
		return config.LockIdentity{}, errors.New("a bot token is required to identify the Slack app to lock")
	}
	authCtx, cancel := context.WithTimeout(ctx, slackIdentityTimeout)
	defer cancel()

	res, err := slack.New(botToken).AuthTestContext(authCtx)
	if err != nil {
		return config.LockIdentity{}, fmt.Errorf("auth.test: %w", err)
	}
	// auth.test does not return an app_id, but bot_id is the identifier we
	// actually want: it names this bot's INSTALLATION in this workspace. It
	// survives token rotation (the credential changes, the installation does
	// not) and changes only if the app is reinstalled, which genuinely is a
	// different gateway. There is no fallback — an empty value fails validation
	// and the daemon refuses to start, because guessing a key here would let two
	// deployments collide on one lock or one deployment split across two.
	identity := config.LockIdentity{TeamID: res.TeamID, AppID: strings.TrimSpace(res.BotID)}
	if err := identity.Validate(); err != nil {
		return config.LockIdentity{}, fmt.Errorf("auth.test did not identify the Slack app: %w", err)
	}
	return identity, nil
}

// closeLeaderLocker releases the election lock's backend handle on shutdown.
// The lease itself is released by the runner's stand-down; this frees the
// client.
func (a *Application) closeLeaderLocker(logger *slog.Logger) {
	if a.leaderLocker == nil {
		return
	}
	if err := a.leaderLocker.Close(); err != nil {
		logger.Warn("could not close the leader lock", "error", err)
	}
	a.leaderLocker = nil
}
