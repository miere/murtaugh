package app

import (
	"context"
	"sync"

	"github.com/miere/murtaugh/internal/config"
	gateway "github.com/miere/murtaugh/internal/slack/gateway"
)

// gatewayHolder holds whichever Gateway is currently live.
//
// A configuration reload replaces the Gateway wholesale rather than mutating
// it: agents, MCP servers, tool sets and routing are all decided at
// construction, and half of them own live process trees. Everything that
// outlives a single gateway — the election runner, the interaction router's
// callbacks — therefore reaches the current one through this, not through a
// captured pointer that a reload would leave dangling.
type gatewayHolder struct {
	mu sync.RWMutex
	gw *gateway.Gateway
}

func (h *gatewayHolder) get() *gateway.Gateway {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.gw
}

// swap installs gw and returns the one it replaced.
func (h *gatewayHolder) swap(gw *gateway.Gateway) *gateway.Gateway {
	h.mu.Lock()
	defer h.mu.Unlock()
	previous := h.gw
	h.gw = gw
	return previous
}

// runGateway builds the gateway, wires the collaborators that outlive it, and
// runs the election until ctx is cancelled.
//
// The election owns the loop, not the gateway: a reload swaps the gateway
// underneath, and the lock must not change hands merely because the daemon
// rebuilt itself.
func (a *Application) runGateway(ctx context.Context) error {
	holder := &gatewayHolder{}
	holder.swap(a.buildGateway(a.cfg))

	// The claimer and the config store outlive every gateway, so they are
	// attached to whichever one is current rather than baked into the first.
	if err := a.wireRunClaims(ctx, holder); err != nil {
		return err
	}
	defer a.closeJobRuns(a.logger)

	runner, err := a.wireLeaderElection(ctx, holder)
	if err != nil {
		return err
	}
	defer a.closeLeaderLocker(a.logger)

	// The gate needs the runner, and the runner needed the holder, so the
	// gateway learns about leadership last. A reload repeats this for the
	// replacement (see reloadConfig).
	holder.get().WithLeaderElection(runner)
	a.attachAgentSetup(ctx, holder.get(), holder, runner)

	// Close agent backends on the way out, whichever gateway is current by
	// then. A reload has already closed the one it replaced.
	defer func() { holder.get().CloseChatSessions() }()

	// The configuration is watched from here on: an edit is shown to the admin
	// as a diff and adopted only if they approve it. The watcher acts only while
	// this node leads, so a standby never prompts about an edit it is not
	// serving.
	a.startConfigWatch(ctx, holder, runner)

	// Daemon-lifetime work, started BEFORE the election and never stopped by it.
	// The credential warden must keep a standby's Claude Code credential alive:
	// it is scoped to this machine, not to the cluster, and a standby whose
	// credential has lapsed is a failover that promotes a node unable to
	// authenticate.
	holder.get().StartBackground(ctx)
	defer func() { holder.get().StopBackground() }()

	a.logger.Info("starting Slack gateway (Socket Mode)", "config", a.configPath)
	err = runner.Run(ctx)
	if err != nil && ctx.Err() != nil {
		err = nil
	}
	a.logger.Info("Slack gateway stopped")
	return err
}

// reloadConfig applies an approved configuration change without exiting.
//
// This is the soft reload: stop serving, tear the old gateway down, build a new
// one from the new configuration, and start serving again — all while holding
// the leader lock, so the cluster never sees a gap it would fail over into.
//
// It is not "hot" in the sense of swapping a value in place, and it cannot be.
// Agents own backend process trees (ACP adapter → mcp-bridge → claude → MCP
// servers) decided at construction; changing one means rebuilding it, which
// stops whatever it was doing. The approval card says so in as many words,
// because an admin who approves without being told that has been misled.
//
// The gain over a process restart is real but narrow: no launchd round trip, no
// binary re-exec, and the leader lease is held throughout rather than released
// and re-acquired. In-flight agent work is lost either way.
// daemonCtx bounds the REPLACEMENT gateway's serving life and must be the
// daemon's own context, not the caller's. opCtx bounds the teardown and the
// notices, which are ordinary bounded operations.
//
// Separating them is not fastidiousness: an earlier version passed one context
// for both, and the agent-setup form calls this under a 30-second write
// timeout. The rebuilt gateway inherited that deadline, connected to Slack, and
// was cancelled 85 milliseconds later — leaving a daemon that held the leader
// lock, looked healthy in the log, and answered nobody.
func (a *Application) reloadConfig(daemonCtx, opCtx context.Context, holder *gatewayHolder, runner leaderRunner, cfg config.Config) error {
	ctx := opCtx
	old := holder.get()

	// Tell the admin before going quiet, using the gateway that can still
	// speak: the stand-down below shuts the outbound gate.
	// Where the "reloading…" notice landed, so the completion can edit it in
	// place rather than post a second block under the card that caused it.
	noticeChannel, noticeTS := old.NotifyConfigReloading(ctx)

	old.StopServing(ctx, "reloading the approved configuration")
	old.StopBackground()
	old.CloseChatSessions()

	replacement := a.buildGateway(cfg)
	replacement.WithLeaderElection(runner)
	a.attachRunClaims(replacement)
	a.attachAgentSetup(daemonCtx, replacement, holder, runner)
	holder.swap(replacement)
	a.cfg = cfg

	replacement.StartBackground(daemonCtx)
	if err := replacement.StartServing(daemonCtx); err != nil {
		// The daemon is now holding the lock without serving, which is the one
		// state worse than either side of the reload. Report it loudly; the
		// runner's next renewal keeps the lock, and an operator restarting the
		// process is the recovery.
		return err
	}
	replacement.NotifyConfigReloaded(ctx, noticeChannel, noticeTS)
	a.logger.Info("configuration reloaded", "config", a.configPath)
	return nil
}

// leaderRunner is the slice of election.Runner this file needs, named so the
// reload path can be tested without standing up a real election.
type leaderRunner interface {
	Run(ctx context.Context) error
	Allow(ctx context.Context) bool
	Leading() bool
}
