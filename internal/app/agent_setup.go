package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/onboarding"
	gateway "github.com/miere/murtaugh/internal/slack/gateway"
	setupenv "github.com/miere/murtaugh/internal/tools/setup/env"
)

// This file applies a completed agent-setup form.
//
// It is the only place that knows about all three halves — the form's output,
// the config store, and the .env file — which is why the write lives here
// rather than in the gateway.

// newAgentProfileWriter builds the closure that turns a completed form into a
// configured, reloaded daemon.
//
// The order matters. The credential is written to .env FIRST, because the
// profiles reference it by variable name and a profile whose key is not yet on
// disk would fail to build the moment the reload picked it up. Then the
// profiles and the routing go into the store, and the reload adopts them.
func (a *Application) newAgentProfileWriter(holder *gatewayHolder, runner leaderRunner) gateway.AgentProfileWriter {
	return func(ctx context.Context, profiles onboarding.Profiles) error {
		if a.cfgStore == nil {
			return fmt.Errorf("no configuration store is open")
		}

		if profiles.EnvKey != "" {
			if err := a.writeEnvVar(ctx, profiles.EnvKey, profiles.EnvValue); err != nil {
				return fmt.Errorf("store the API key: %w", err)
			}
		}

		if err := a.cfgStore.UpsertItem(ctx, config.SectionAgent, profiles.Name, profiles.Default); err != nil {
			return fmt.Errorf("save the %q agent: %w", profiles.Name, err)
		}
		if err := a.cfgStore.UpsertItem(ctx, config.SectionAgent, onboarding.TweakerName, profiles.Tweaker); err != nil {
			return fmt.Errorf("save the %q agent: %w", onboarding.TweakerName, err)
		}
		// Chat last: it references both agents, so writing it first would leave
		// a window in which the store holds a configuration that does not load.
		if err := a.cfgStore.PutSingleton(ctx, config.SingletonChat, profiles.Chat); err != nil {
			return fmt.Errorf("enable chat: %w", err)
		}

		return a.adoptOwnConfigChange(ctx, holder, runner)
	}
}

// adoptOwnConfigChange reloads onto a configuration Murtaugh itself just wrote.
//
// It re-baselines the watcher before reloading, which is the whole point: the
// watcher exists to catch changes the daemon did not make, and without this it
// would spot these writes on its next poll and ask the administrator to approve
// a change they made thirty seconds ago through a form. Asking somebody to
// re-approve their own click is how a review prompt becomes noise.
func (a *Application) adoptOwnConfigChange(ctx context.Context, holder *gatewayHolder, runner leaderRunner) error {
	snapshot, err := a.cfgStore.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("read back the configuration: %w", err)
	}
	cfg, err := a.cfgStore.Load(ctx, a.cfg)
	if err != nil {
		return fmt.Errorf("the new configuration does not load: %w", err)
	}

	// Baseline BEFORE the reload: the reload tears this gateway down and
	// rebuilds it, and a watcher poll landing in that window would otherwise
	// see the change as unreviewed.
	a.setApprovedConfig(snapshot)

	return a.reloadConfig(ctx, holder, runner, cfg)
}

// writeEnvVar stores a credential in the .env beside the config.
//
// It goes through the setup.env tool rather than writing the file directly.
// That tool already owns the merge semantics — preserve unrelated keys, back up
// the previous file — and its envfile package sits behind an internal/ boundary
// this package cannot import anyway.
func (a *Application) writeEnvVar(ctx context.Context, key, value string) error {
	if strings.TrimSpace(a.configPath) == "" {
		return fmt.Errorf("the config path is unknown, so there is no .env to write")
	}
	envPath := filepath.Join(filepath.Dir(a.configPath), ".env")
	tool := setupenv.New(func() string { return envPath })
	_, err := tool.Invoke(ctx, map[string]any{"set": []any{key + "=" + value}})
	return err
}

// setApprovedConfig re-baselines the hot-reload watcher.
func (a *Application) setApprovedConfig(snapshot config.Snapshot) {
	a.approvedCfgMu.Lock()
	defer a.approvedCfgMu.Unlock()
	a.approvedCfg = snapshot
	a.approvedCfgSet = true
}

// takeApprovedConfig returns a baseline set elsewhere, and whether there was
// one. The watcher calls it each tick so a configuration Murtaugh wrote itself
// silently becomes the new baseline instead of being queued for review.
func (a *Application) takeApprovedConfig() (config.Snapshot, bool) {
	a.approvedCfgMu.Lock()
	defer a.approvedCfgMu.Unlock()
	if !a.approvedCfgSet {
		return config.Snapshot{}, false
	}
	a.approvedCfgSet = false
	return a.approvedCfg, true
}

// hasAgents reports whether any agent profile is configured.
func hasAgents(cfg config.Config) bool { return len(cfg.Agents) > 0 }

// attachAgentSetup gives a gateway the ability to run the setup form.
//
// Repeated for every rebuilt gateway, like the run claimer: a reload replaces
// the object, and a replacement that could not offer the form would strand an
// operator who had not finished onboarding.
func (a *Application) attachAgentSetup(gw *gateway.Gateway, holder *gatewayHolder, runner leaderRunner) {
	if gw == nil {
		return
	}
	gw.WithAgentProfileWriter(a.newAgentProfileWriter(holder, runner))
}
