package app

import (
	"context"
	"strings"

	"github.com/miere/murtaugh/internal/config"
	gateway "github.com/miere/murtaugh/internal/slack/gateway"
	setupupdate "github.com/miere/murtaugh/internal/tools/setup/update"
	"github.com/miere/murtaugh/internal/troubleshoot"
	"github.com/miere/murtaugh/internal/updates"
)

// buildGateway constructs a Gateway from cfg and wires every collaborator the
// daemon needs, EXCEPT leader election and the scheduled-run claimer — those
// two outlive any single gateway and are attached by the caller.
//
// It takes cfg as a parameter rather than reading a.cfg because a configuration
// reload builds a second gateway from the newly approved configuration while
// the old one is still being torn down. Everything an agent owns — session
// managers, MCP servers, tool sets, routing — is decided here at construction,
// which is precisely why a reload has to rebuild rather than mutate.
func (a *Application) buildGateway(cfg config.Config) *gateway.Gateway {
	gw := gateway.New(cfg, a.registry, a.logger, a.recorder, a.interactionBroker, a.authFlow, a.askFlow)
	if rc := a.restart; rc != nil {
		// Adapt the coordinator's Request method into the gateway's
		// stringly-typed trigger so the gateway package stays free
		// of any internal/app import (which would cycle).
		gw = gw.WithRestartTrigger(func(source, userID, channel, reason string) bool {
			return rc.Request(RestartRequest{
				Source:  RestartSource(source),
				UserID:  userID,
				Channel: channel,
				Reason:  reason,
			})
		})
	}
	if path := strings.TrimSpace(a.resumeMarkerPath); path != "" {
		gw = gw.WithResumeMarkerStore(gateway.NewFileResumeMarkerStore(path))
		a.logger.Debug("resume marker store wired", "path", path)
	}
	// Scheduled jobs reuse the jobs.run execution path so a cron/every
	// run behaves identically to a manual one (same timeout, workdir, and
	// exit-code handling). Output streams to the daemon's stdout/stderr,
	// which launchd captures into the Murtaugh log files.
	gw = gw.WithScheduledRunner(newScheduledRunner(cfg, a.recorder, a.registry))
	// Approving a held job's first run writes `confirmed: true` back to the
	// store, so the prompt is not repeated after every restart. The gate
	// still re-arms on change: every job write surface (jobs.define,
	// cfg.job.set) stamps the entry unconfirmed again.
	if a.cfgStore != nil {
		gw = gw.WithJobConfirmer(newJobConfirmer(a.cfgStore))
		// First-user-wins adoption of a fresh install. Without a store the
		// claim still works for this process but is forgotten on restart.
		gw = gw.WithAdminClaimer(newAdminClaimer(a.cfgStore))
	}
	if a.journalSweep != nil {
		gw = gw.WithJournalSweeper(a.journalSweep, a.journalSweepEvery)
	}
	// `/murtaugh troubleshoot <symptoms>` assembles a redacted diagnostics
	// bundle and DMs it to the admin. The gateway owns Slack delivery; the
	// deterministic file assembly is this closure over the same bundler the
	// troubleshoot.bundle tool uses. Always attempts to include known
	// providers (e.g. Goose) — absent files are simply skipped.
	gw = gw.WithTroubleshootBundler(func(ctx context.Context, note string) (string, []string, error) {
		res, err := troubleshoot.Build(ctx, troubleshoot.Options{
			Note:      note,
			Providers: effectiveTroubleshootProviders(cfg),
		}, troubleshoot.ResolveSources(
			cfg.Journal.EffectivePath(cfg.BaseDir, cfg.BaseName),
			cfg.Journal.EffectiveBlobDir(cfg.BaseDir, cfg.BaseName),
			baseDirFor(cfg, a.configPath),
			a.version,
		))
		if err != nil {
			return "", nil, err
		}
		return res.Path, res.Manifest.Errors, nil
	})
	// App Home control panel: everyone who opens the Home tab sees the
	// running version; the admin additionally gets a one-click "Update"
	// button when a newer release exists. The check reuses the same release
	// source as the setup.update tool, and the install path IS that tool,
	// followed by the existing restart coordinator.
	updDeps := updateDeps(a.version)
	gw = gw.WithVersion(a.version).WithUpdateChecker(
		updates.New(updates.Deps{
			Current: a.version,
			Owner:   updDeps.Owner,
			Repo:    updDeps.Repo,
			HTTPGet: updates.HTTPGet(updDeps.HTTPGet),
		}),
		func(ctx context.Context, target string) (string, error) {
			args := map[string]any{}
			if t := strings.TrimSpace(target); t != "" {
				args["version"] = t
			}
			out, err := setupupdate.New(updDeps).Invoke(ctx, args)
			if err != nil {
				return "", err
			}
			res, _ := out.(setupupdate.Result)
			return res.TargetVersion, nil
		},
	)
	return gw
}
