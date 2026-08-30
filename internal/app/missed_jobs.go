package app

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/miere/murtaugh/internal/config"
	gateway "github.com/miere/murtaugh/internal/slack/gateway"
)

// This file reports scheduled runs that did not happen.
//
// It deliberately reports rather than replays. Murtaugh does not promise that a
// cron fires across a leadership change: gocron computes the NEXT occurrence
// from whenever its scheduler started, so an occurrence that falls inside a
// failover or a reload is simply gone. Guaranteeing otherwise is a job for a
// scheduler built for it (Cloud Scheduler, a remote runner), not for a chat
// gateway that happens to own a cron table.
//
// Replaying is also not a decision Murtaugh can make from a cron string. A
// 03:00 backup that runs at 09:00 is fine; a "post the standup reminder" that
// runs at 14:00 is actively harmful, and both look identical here. So the miss
// is surfaced to the admin, who knows which it is and can re-fire it with
// jobs.run — turning a silent gap into a visible one without inventing policy.

// missedJobGrace is how long past an occurrence before it counts as missed.
//
// Cron resolves to the minute, and a promotion may land moments before a run
// that is about to happen normally. Two minutes is comfortably past that
// without being long enough to hide a real miss.
const missedJobGrace = 2 * time.Minute

// missedJobScanLimit bounds how many occurrences are counted for one job, so a
// per-minute cron that has not run for a month cannot spin here.
const missedJobScanLimit = 500

// reportMissedJobs looks for scheduled runs that should have happened and did
// not, and warns the admin about them.
//
// It runs on promotion, which is the moment the question is interesting: this
// node is taking over, and whatever the previous leader failed to run is now
// visible for the first time.
func (a *Application) reportMissedJobs(ctx context.Context, gw *gateway.Gateway) {
	if a.jobRuns == nil || gw == nil {
		return
	}
	missed := a.findMissedJobs(ctx, time.Now().UTC())
	if len(missed) == 0 {
		return
	}
	a.logger.Warn("scheduled runs were missed", "jobs", len(missed))
	gw.NotifyMissedJobs(ctx, missed)
}

// findMissedJobs returns the cron jobs with at least one unrun occurrence.
func (a *Application) findMissedJobs(ctx context.Context, now time.Time) []gateway.MissedJob {
	var missed []gateway.MissedJob

	for name, job := range a.cfg.Jobs {
		// Interval jobs are skipped, and not by oversight: `every: 1h` has no
		// absolute schedule, so there is no occurrence to have missed. A new
		// leader simply starts a fresh interval, which is the intended
		// behaviour rather than a gap.
		if job.ScheduleKind() != config.ScheduleCron {
			continue
		}
		schedule, err := cron.ParseStandard(strings.TrimSpace(job.Schedule))
		if err != nil {
			// The scheduler already logged and skipped this job at
			// registration; repeating the complaint here helps nobody.
			continue
		}

		lastRun, ok, err := a.jobRuns.LastRun(ctx, name)
		if err != nil {
			a.logger.Warn("could not read a job's last run", "job", name, "error", err)
			continue
		}
		// No run on record at all. That is a job which has never fired, and we
		// cannot tell "created five minutes ago" from "never once ran" — so
		// claiming it was missed would greet every fresh deployment with a
		// warning about jobs that were never due.
		if !ok {
			continue
		}

		if report, found := missedSince(name, schedule, lastRun, now); found {
			missed = append(missed, report)
		}
	}

	// Stable order so the same outage produces the same message twice rather
	// than a reshuffled one.
	sort.Slice(missed, func(i, j int) bool { return missed[i].Name < missed[j].Name })
	return missed
}

// missedSince counts the occurrences due between lastRun and now.
//
// Counting forward from the last CLAIMED run is what makes this one cheap
// question rather than a scan: everything before that point demonstrably ran,
// so the only occurrences that can have been missed are the ones after it.
func missedSince(name string, schedule cron.Schedule, lastRun, now time.Time) (gateway.MissedJob, bool) {
	cutoff := now.Add(-missedJobGrace)

	first := schedule.Next(lastRun)
	if !first.Before(cutoff) {
		return gateway.MissedJob{}, false
	}

	count := 0
	latest := first
	for at := first; at.Before(cutoff) && count < missedJobScanLimit; at = schedule.Next(at) {
		latest = at
		count++
	}

	return gateway.MissedJob{
		Name:     name,
		LastRun:  lastRun,
		Expected: latest,
		Count:    count,
		Capped:   count >= missedJobScanLimit,
	}, true
}
