package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/miere/murtaugh/internal/slack/alertcard"
)

// MissedJob is one scheduled job with occurrences that did not run.
type MissedJob struct {
	// Name is the configured job name, as `jobs.run` takes it.
	Name string
	// Expected is the most recent occurrence that should have run.
	Expected time.Time
	// LastRun is the newest occurrence that demonstrably did run.
	LastRun time.Time
	// Count is how many occurrences were missed between the two.
	Count int
	// Capped reports that counting stopped at the scan limit, so Count is a
	// floor rather than the total.
	Capped bool
}

// NotifyMissedJobs warns the admin about scheduled runs that did not happen.
//
// It is a WARNING rather than an INFO because something the operator asked for
// did not occur and may need doing by hand — which is exactly the line
// alertcard draws between the two levels. It is not an error: nothing broke,
// and Murtaugh never promised these would survive a leadership change.
//
// Best-effort, like every other alert here: a node that cannot report a missed
// job is still a working leader.
func (a *Gateway) NotifyMissedJobs(ctx context.Context, missed []MissedJob) {
	if len(missed) == 0 {
		return
	}
	admin := strings.TrimSpace(a.cfg.AdminUser)
	if admin == "" {
		return
	}
	if _, _, err := a.postLifecycleAlert(ctx, admin, "", missedJobsAlert(missed)); err != nil {
		a.logger.Warn("could not report missed scheduled runs", "error", err)
	}
}

// missedJobsAlert renders the warning.
//
// The next step names `jobs.run` explicitly. The whole reason Murtaugh reports
// rather than replays is that only the admin knows whether a late run is
// wanted, so the message has to hand them the means to make that call rather
// than leaving them to go and find it.
func missedJobsAlert(missed []MissedJob) alertcard.Spec {
	return alertcard.Spec{
		Level:     alertcard.LevelWarn,
		Title:     missedJobsTitle(missed),
		Subtitle:  "A leadership change or a restart fell across their scheduled time.",
		Text:      "Murtaugh does not re-run missed schedules on its own: whether a late run is wanted depends on the job, and only you can say.",
		Detail:    missedJobsDetail(missed),
		NextSteps: "Re-run anything that still needs doing with `murtaugh jobs run <name>`.",
	}
}

func missedJobsTitle(missed []MissedJob) string {
	if len(missed) == 1 {
		return "A scheduled job did not run"
	}
	return fmt.Sprintf("%d scheduled jobs did not run", len(missed))
}

func missedJobsDetail(missed []MissedJob) string {
	lines := make([]string, 0, len(missed))
	for _, m := range missed {
		lines = append(lines, describeMissedJobLine(m))
	}
	return strings.Join(lines, "\n")
}

// describeMissedJobLine renders one job's miss.
func describeMissedJobLine(m MissedJob) string {
	last := m.LastRun.UTC().Format(time.RFC3339)
	when := m.Expected.UTC().Format(time.RFC3339)
	if m.Count <= 1 {
		return fmt.Sprintf("%s — due %s, did not run (last ran %s)", m.Name, when, last)
	}
	count := fmt.Sprintf("%d", m.Count)
	if m.Capped {
		count = "at least " + count
	}
	return fmt.Sprintf("%s — %s runs missed, most recently %s (last ran %s)", m.Name, count, when, last)
}
