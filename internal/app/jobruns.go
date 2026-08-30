package app

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/store"
	gateway "github.com/miere/murtaugh/internal/slack/gateway"
)

// wireRunClaims gives the gateway's scheduler a shared claim store, so a
// scheduled occurrence runs at most once across every node and every restart.
//
// Like leader election this is not optional and its failure is fatal. The
// degraded state is a job running twice — and the jobs Murtaugh schedules are
// arbitrary shell commands or agent turns, so "twice" can mean two deploys, two
// mails, or two rewrites of a repository. Starting a scheduler that cannot tell
// whether a run already happened is worse than not starting.
//
// It is skipped only when nothing would use it: a gateway with no scheduled
// jobs never calls the claimer, and opening a second database handle for it
// would be pure cost.
func (a *Application) wireRunClaims(ctx context.Context, gw *gateway.Gateway) (*gateway.Gateway, error) {
	if !hasScheduledJobs(a.cfg) {
		return gw, nil
	}

	runs, err := store.OpenJobRuns(ctx, a.cfg.Database, filepath.Dir(a.configPath), config.BaseNameOf(a.configPath))
	if err != nil {
		return nil, fmt.Errorf("scheduled-run claims: %w", err)
	}
	a.jobRuns = runs

	// Sweep once at startup rather than on a ticker. Claims are only ever
	// compared against the present, so stale rows cost storage and nothing
	// else; a leader that has just promoted is exactly the moment to pay that
	// small cost, and a node that never promotes never pays it.
	a.pruneJobRuns(ctx, runs)

	return gw.WithRunClaimer(func(ctx context.Context, claim config.JobRunClaim) (bool, error) {
		return runs.Claim(ctx, claim, config.OwnerID())
	}), nil
}

// hasScheduledJobs reports whether any job is cron- or interval-driven.
func hasScheduledJobs(cfg config.Config) bool {
	for _, job := range cfg.Jobs {
		if job.ScheduleKind() != config.ScheduleManual {
			return true
		}
	}
	return false
}

// pruneJobRuns drops claims past the retention window. Best-effort: failing to
// tidy up is not a reason to refuse to run jobs.
func (a *Application) pruneJobRuns(ctx context.Context, runs config.JobRunStore) {
	pruneCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	deleted, err := runs.Prune(pruneCtx, time.Now().UTC().Add(-config.JobRunRetention))
	if err != nil {
		a.logger.Warn("could not prune old scheduled-run claims", "error", err)
		return
	}
	if deleted > 0 {
		a.logger.Info("pruned old scheduled-run claims", "claims", deleted)
	}
}

// closeJobRuns releases the claim store's handle on shutdown.
func (a *Application) closeJobRuns(logger *slog.Logger) {
	if a.jobRuns == nil {
		return
	}
	if err := a.jobRuns.Close(); err != nil {
		logger.Warn("could not close the scheduled-run claim store", "error", err)
	}
	a.jobRuns = nil
}
