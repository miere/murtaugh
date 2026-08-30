package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

// claimRecorder is a scriptable RunClaimer that records what it was asked.
type claimRecorder struct {
	mu     sync.Mutex
	claims []config.JobRunClaim
	grant  bool
	err    error
}

func (c *claimRecorder) claimer() RunClaimer {
	return func(_ context.Context, claim config.JobRunClaim) (bool, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.claims = append(c.claims, claim)
		return c.grant, c.err
	}
}

func (c *claimRecorder) seen() []config.JobRunClaim {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]config.JobRunClaim(nil), c.claims...)
}

// claimTestGateway builds the minimum Gateway claimScheduledRun touches, with a
// frozen clock so the computed occurrence is deterministic.
func claimTestGateway(jobs map[string]config.JobProfile, at time.Time, claimer RunClaimer) *Gateway {
	return &Gateway{
		logger:        quietLogger(),
		scheduledJobs: jobs,
		claimRun:      claimer,
		now:           func() time.Time { return at },
	}
}

var claimNow = time.Date(2026, 8, 30, 9, 0, 37, 500_000_000, time.UTC)

// TestClaimGrantedRunsTheJob is the ordinary path: the slot was free, so this
// node runs it.
func TestClaimGrantedRunsTheJob(t *testing.T) {
	rec := &claimRecorder{grant: true}
	gw := claimTestGateway(map[string]config.JobProfile{
		"nightly": {Command: "echo", Schedule: "0 9 * * *"},
	}, claimNow, rec.claimer())

	if !gw.claimScheduledRun(context.Background(), "nightly") {
		t.Fatal("a granted claim did not permit the run")
	}
	claims := rec.seen()
	if len(claims) != 1 {
		t.Fatalf("%d claims made, want 1", len(claims))
	}
	if claims[0].Job != "nightly" {
		t.Errorf("claimed job %q, want nightly", claims[0].Job)
	}
}

// TestClaimRefusedSkipsTheJob covers the case the whole mechanism exists for: a
// restart or a failover landing inside a job's window must not run it again.
func TestClaimRefusedSkipsTheJob(t *testing.T) {
	rec := &claimRecorder{grant: false}
	gw := claimTestGateway(map[string]config.JobProfile{
		"nightly": {Command: "echo", Schedule: "0 9 * * *"},
	}, claimNow, rec.claimer())

	if gw.claimScheduledRun(context.Background(), "nightly") {
		t.Fatal("a job whose occurrence was already claimed ran a second time")
	}
}

// TestClaimErrorSkipsTheJob pins the direction of the error bias. Not knowing
// whether a slot is taken is not permission to take it: a skipped run is
// visible and recovers next tick, a duplicate deploy or duplicate mail may not.
func TestClaimErrorSkipsTheJob(t *testing.T) {
	rec := &claimRecorder{grant: true, err: errors.New("store unreachable")}
	gw := claimTestGateway(map[string]config.JobProfile{
		"nightly": {Command: "echo", Schedule: "0 9 * * *"},
	}, claimNow, rec.claimer())

	if gw.claimScheduledRun(context.Background(), "nightly") {
		t.Fatal("a job ran despite the claim store being unreachable")
	}
}

// TestClaimIsSkippedWithoutAClaimer keeps CLI/MCP and struct-literal gateways
// working: with nothing wired the scheduler behaves exactly as it did before.
func TestClaimIsSkippedWithoutAClaimer(t *testing.T) {
	gw := claimTestGateway(map[string]config.JobProfile{
		"nightly": {Command: "echo", Schedule: "0 9 * * *"},
	}, claimNow, nil)

	if !gw.claimScheduledRun(context.Background(), "nightly") {
		t.Fatal("an unwired gateway blocked its own scheduled job")
	}
}

// TestClaimOccurrenceMatchesTheScheduleKind is the property that makes two
// nodes agree.
//
// A cron job resolves to the minute, so every node firing in the 09:00 minute
// computes the same slot and exactly one wins. An interval job resolves to its
// own period, so a leader that restarts 20 minutes into an hourly job lands on
// the same slot it already claimed and stands down.
func TestClaimOccurrenceMatchesTheScheduleKind(t *testing.T) {
	for name, tc := range map[string]struct {
		job  config.JobProfile
		want time.Time
	}{
		"cron truncates to the minute": {
			job:  config.JobProfile{Command: "echo", Schedule: "0 9 * * *"},
			want: time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC),
		},
		"hourly interval truncates to the hour": {
			job:  config.JobProfile{Command: "echo", Every: "1h"},
			want: time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC),
		},
		"15m interval truncates to the quarter": {
			job:  config.JobProfile{Command: "echo", Every: "15m"},
			want: time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC),
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec := &claimRecorder{grant: true}
			gw := claimTestGateway(map[string]config.JobProfile{"j": tc.job}, claimNow, rec.claimer())

			if !gw.claimScheduledRun(context.Background(), "j") {
				t.Fatal("claim refused")
			}
			claims := rec.seen()
			if len(claims) != 1 {
				t.Fatalf("%d claims, want 1", len(claims))
			}
			if !claims[0].Occurrence.Equal(tc.want) {
				t.Errorf("occurrence = %v, want %v", claims[0].Occurrence, tc.want)
			}
		})
	}
}

// TestClaimSlotIsIndependentOfNodeUptime is the trap this design avoids.
//
// The obvious slot to use is "time since the scheduler started", because that
// is what the scheduler itself counts. It is also useless: every node starts at
// a different moment, so every node computes its own grid and every node wins
// its own claim. Truncating wall-clock time against a fixed origin is what
// makes two nodes agree, and this asserts that two different "nodes" firing
// moments apart land on one slot.
func TestClaimSlotIsIndependentOfNodeUptime(t *testing.T) {
	job := map[string]config.JobProfile{"j": {Command: "echo", Schedule: "0 9 * * *"}}

	early := &claimRecorder{grant: true}
	late := &claimRecorder{grant: true}

	// Two nodes fire 20 seconds apart, inside the same cron minute.
	claimTestGateway(job, claimNow, early.claimer()).claimScheduledRun(context.Background(), "j")
	claimTestGateway(job, claimNow.Add(20*time.Second), late.claimer()).claimScheduledRun(context.Background(), "j")

	a, b := early.seen(), late.seen()
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("expected one claim each, got %d and %d", len(a), len(b))
	}
	if !a[0].Occurrence.Equal(b[0].Occurrence) {
		t.Errorf("two nodes computed different slots for one cron minute: %v vs %v; each would win its own claim",
			a[0].Occurrence, b[0].Occurrence)
	}
}

// TestManualJobNeedsNoClaim checks a job with no schedule is left alone: it has
// no occurrence to contend for, and gating it would break jobs.run.
func TestManualJobNeedsNoClaim(t *testing.T) {
	rec := &claimRecorder{grant: false}
	gw := claimTestGateway(map[string]config.JobProfile{
		"manual": {Command: "echo"},
	}, claimNow, rec.claimer())

	if !gw.claimScheduledRun(context.Background(), "manual") {
		t.Fatal("a manual job was gated by a scheduled-run claim")
	}
	if len(rec.seen()) != 0 {
		t.Errorf("a manual job consulted the claim store %d times", len(rec.seen()))
	}
}

// TestUnknownJobNeedsNoClaim guards the lookup: a name with no profile must not
// be silently gated by a zero-valued schedule.
func TestUnknownJobNeedsNoClaim(t *testing.T) {
	rec := &claimRecorder{grant: false}
	gw := claimTestGateway(map[string]config.JobProfile{}, claimNow, rec.claimer())

	if !gw.claimScheduledRun(context.Background(), "missing") {
		t.Fatal("an unknown job was gated")
	}
}
