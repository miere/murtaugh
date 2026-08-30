package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/store"
)

// missedJobsApp builds an Application with a real claim store and the given
// jobs configured.
func missedJobsApp(t *testing.T, jobs map[string]config.JobProfile) (*Application, config.JobRunStore) {
	t.Helper()
	runs, err := store.OpenJobRuns(context.Background(), config.DatabaseConfig{
		Backend: config.BackendSQLite,
		SQLite:  config.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")},
	}, "", "")
	if err != nil {
		t.Fatalf("OpenJobRuns: %v", err)
	}
	t.Cleanup(func() { _ = runs.Close() })

	return &Application{
		logger:  quietLogger(),
		cfg:     config.Config{Jobs: jobs},
		jobRuns: runs,
	}, runs
}

var nineAM = config.JobProfile{Command: "echo", Schedule: "0 9 * * *"}

// claimAt records a run so the detector has a baseline to count from.
func claimAt(t *testing.T, runs config.JobRunStore, job string, at time.Time) {
	t.Helper()
	ok, err := runs.Claim(context.Background(), config.JobRunClaim{
		Job:        job,
		Occurrence: config.OccurrenceFor(at, time.Minute),
	}, "test-node")
	if err != nil || !ok {
		t.Fatalf("claim %s at %v: ok=%v err=%v", job, at, ok, err)
	}
}

// TestMissedRunIsDetected is the case the feature exists for: a failover or a
// reload fell across the job's minute, so the occurrence never ran.
func TestMissedRunIsDetected(t *testing.T) {
	app, runs := missedJobsApp(t, map[string]config.JobProfile{"backup": nineAM})

	// It ran yesterday at 09:00; today's 09:00 came and went.
	claimAt(t, runs, "backup", time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC))
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)

	missed := app.findMissedJobs(context.Background(), now)
	if len(missed) != 1 {
		t.Fatalf("%d missed jobs, want 1: %+v", len(missed), missed)
	}
	if missed[0].Name != "backup" {
		t.Errorf("missed job = %q, want backup", missed[0].Name)
	}
	want := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)
	if !missed[0].Expected.Equal(want) {
		t.Errorf("expected occurrence = %v, want %v", missed[0].Expected, want)
	}
	if missed[0].Count != 1 {
		t.Errorf("count = %d, want 1", missed[0].Count)
	}
}

// TestRunOnScheduleIsNotReported pins the quiet path. This fires on every
// promotion, so a false positive would train the admin to ignore the warning.
func TestRunOnScheduleIsNotReported(t *testing.T) {
	app, runs := missedJobsApp(t, map[string]config.JobProfile{"backup": nineAM})

	claimAt(t, runs, "backup", time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC))
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)

	if missed := app.findMissedJobs(context.Background(), now); len(missed) != 0 {
		t.Errorf("a job that ran on time was reported missed: %+v", missed)
	}
}

// TestGracePeriodCoversAnImminentRun stops a promotion landing seconds before a
// due job from reporting it as missed — it is about to run normally.
func TestGracePeriodCoversAnImminentRun(t *testing.T) {
	app, runs := missedJobsApp(t, map[string]config.JobProfile{"backup": nineAM})

	claimAt(t, runs, "backup", time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC))
	// Thirty seconds past 09:00 today: due, not yet run, and well inside grace.
	now := time.Date(2026, 8, 30, 9, 0, 30, 0, time.UTC)

	if missed := app.findMissedJobs(context.Background(), now); len(missed) != 0 {
		t.Errorf("a job still inside its grace window was reported missed: %+v", missed)
	}
}

// TestNeverRunJobIsNotReported covers a fresh deployment. A job with no claim
// on record cannot be told apart from one created five minutes ago, so warning
// about it would greet every new install with alarms about jobs that were never
// due.
func TestNeverRunJobIsNotReported(t *testing.T) {
	app, _ := missedJobsApp(t, map[string]config.JobProfile{"backup": nineAM})
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)

	if missed := app.findMissedJobs(context.Background(), now); len(missed) != 0 {
		t.Errorf("a job that has never run was reported missed: %+v", missed)
	}
}

// TestIntervalJobsAreNotReported pins the deliberate exclusion: `every: 1h` has
// no absolute schedule, so there is no occurrence to have missed. A new leader
// starting a fresh interval is the intended behaviour, not a gap.
func TestIntervalJobsAreNotReported(t *testing.T) {
	app, runs := missedJobsApp(t, map[string]config.JobProfile{
		"hourly": {Command: "echo", Every: "1h"},
	})
	claimAt(t, runs, "hourly", time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC))
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)

	if missed := app.findMissedJobs(context.Background(), now); len(missed) != 0 {
		t.Errorf("an interval job was reported missed: %+v", missed)
	}
}

// TestMultipleMissesAreCounted covers a longer outage: the admin should learn
// how much did not happen, not merely that something did not.
func TestMultipleMissesAreCounted(t *testing.T) {
	app, runs := missedJobsApp(t, map[string]config.JobProfile{"backup": nineAM})

	claimAt(t, runs, "backup", time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC))
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)

	missed := app.findMissedJobs(context.Background(), now)
	if len(missed) != 1 {
		t.Fatalf("%d missed jobs, want 1", len(missed))
	}
	// 27th, 28th, 29th, 30th.
	if missed[0].Count != 4 {
		t.Errorf("count = %d, want 4", missed[0].Count)
	}
	if want := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC); !missed[0].Expected.Equal(want) {
		t.Errorf("most recent missed occurrence = %v, want %v", missed[0].Expected, want)
	}
}

// TestScanIsBounded guards the counting loop. A per-minute cron that has not
// run for a month would otherwise walk half a million occurrences on a path
// that runs during promotion.
func TestScanIsBounded(t *testing.T) {
	schedule, err := cron.ParseStandard("* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	lastRun := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)

	report, found := missedSince("minutely", schedule, lastRun, now)
	if !found {
		t.Fatal("a job stopped for months was not reported missed")
	}
	if report.Count != missedJobScanLimit {
		t.Errorf("count = %d, want the scan limit %d", report.Count, missedJobScanLimit)
	}
	if !report.Capped {
		t.Error("a capped count was not flagged as a floor; the admin would read it as the total")
	}
}

// TestReportIsOrdered keeps repeated messages about one outage identical rather
// than reshuffled, which is what map iteration would otherwise produce.
func TestReportIsOrdered(t *testing.T) {
	app, runs := missedJobsApp(t, map[string]config.JobProfile{
		"zulu":  nineAM,
		"alpha": nineAM,
		"mike":  nineAM,
	})
	for _, name := range []string{"zulu", "alpha", "mike"} {
		claimAt(t, runs, name, time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC))
	}
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		missed := app.findMissedJobs(context.Background(), now)
		if len(missed) != 3 {
			t.Fatalf("%d missed jobs, want 3", len(missed))
		}
		for j, want := range []string{"alpha", "mike", "zulu"} {
			if missed[j].Name != want {
				t.Fatalf("run %d: missed[%d] = %q, want %q", i, j, missed[j].Name, want)
			}
		}
	}
}

// TestMalformedScheduleIsSkipped checks a job the scheduler already refused
// does not crash or double-report here.
func TestMalformedScheduleIsSkipped(t *testing.T) {
	app, runs := missedJobsApp(t, map[string]config.JobProfile{
		"broken": {Command: "echo", Schedule: "not a cron expression"},
	})
	claimAt(t, runs, "broken", time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC))
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)

	if missed := app.findMissedJobs(context.Background(), now); len(missed) != 0 {
		t.Errorf("a job with an unparseable schedule was reported: %+v", missed)
	}
}
