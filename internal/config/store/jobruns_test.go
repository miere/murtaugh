package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

// jobRunsFactory builds a claim store; every store a factory returns points at
// the same data, which is how these tests stand in for separate nodes.
type jobRunsFactory func(t *testing.T) config.JobRunStore

func sqliteJobRunsFactory(t *testing.T) jobRunsFactory {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.db")
	return func(t *testing.T) config.JobRunStore {
		t.Helper()
		runs, err := OpenJobRuns(context.Background(),
			config.DatabaseConfig{Backend: config.BackendSQLite, SQLite: config.SQLiteConfig{Path: path}}, "", "")
		if err != nil {
			t.Fatalf("OpenJobRuns(sqlite): %v", err)
		}
		t.Cleanup(func() { _ = runs.Close() })
		return runs
	}
}

func postgresJobRunsFactory(t *testing.T) jobRunsFactory {
	t.Helper()
	dsn := postgresTestDSN(t)
	return func(t *testing.T) config.JobRunStore {
		t.Helper()
		runs, err := OpenJobRuns(context.Background(),
			config.DatabaseConfig{Backend: config.BackendPostgres, Postgres: config.PostgresConfig{DSN: dsn}}, "", "")
		if err != nil {
			t.Fatalf("OpenJobRuns(postgres): %v", err)
		}
		t.Cleanup(func() { _ = runs.Close() })
		return runs
	}
}

func firestoreJobRunsFactory(t *testing.T) jobRunsFactory {
	t.Helper()
	fsc := firestoreTestConfig(t)
	return func(t *testing.T) config.JobRunStore {
		t.Helper()
		runs, err := OpenJobRuns(context.Background(),
			config.DatabaseConfig{Backend: config.BackendFirestore, Firestore: fsc}, "", "")
		if err != nil {
			t.Fatalf("OpenJobRuns(firestore): %v", err)
		}
		t.Cleanup(func() { _ = runs.Close() })
		return runs
	}
}

// uniqueJobName keeps cases independent against a shared database, which the
// Postgres and Firestore backends both are between runs.
func uniqueJobName(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), jobNameSeq.Add(1))
}

var jobNameSeq atomic.Int64

// TestJobRunClaims runs the claim contract against every backend that
// implements it. All three ship, so all three are held to the same behaviour.
func TestJobRunClaims(t *testing.T) {
	occurrence := config.OccurrenceFor(time.Date(2026, 8, 30, 9, 0, 30, 0, time.UTC), time.Minute)

	for name, build := range map[string]func(*testing.T) jobRunsFactory{
		"sqlite":    sqliteJobRunsFactory,
		"postgres":  postgresJobRunsFactory,
		"firestore": firestoreJobRunsFactory,
	} {
		t.Run(name, func(t *testing.T) {
			t.Run("a slot is claimable exactly once", func(t *testing.T) {
				newRuns := build(t)
				ctx := context.Background()
				job := uniqueJobName("once")

				first := newRuns(t)
				claimed, err := first.Claim(ctx, config.JobRunClaim{Job: job, Occurrence: occurrence}, "node-a")
				if err != nil || !claimed {
					t.Fatalf("first Claim: claimed=%v err=%v; want the slot", claimed, err)
				}

				// A second node — or the same one after a restart — must be
				// refused, with no error: losing a claim is ordinary.
				second := newRuns(t)
				claimed, err = second.Claim(ctx, config.JobRunClaim{Job: job, Occurrence: occurrence}, "node-b")
				if err != nil {
					t.Fatalf("second Claim errored; a taken slot is not a failure: %v", err)
				}
				if claimed {
					t.Fatal("two nodes both claimed one occurrence; the job would run twice")
				}
			})

			t.Run("different slots do not collide", func(t *testing.T) {
				newRuns := build(t)
				ctx := context.Background()
				runs := newRuns(t)
				job := uniqueJobName("slots")

				for i := 0; i < 3; i++ {
					slot := occurrence.Add(time.Duration(i) * time.Minute)
					claimed, err := runs.Claim(ctx, config.JobRunClaim{Job: job, Occurrence: slot}, "node-a")
					if err != nil || !claimed {
						t.Fatalf("slot %d: claimed=%v err=%v; consecutive occurrences must each be claimable", i, claimed, err)
					}
				}
			})

			t.Run("different jobs do not collide", func(t *testing.T) {
				newRuns := build(t)
				ctx := context.Background()
				runs := newRuns(t)

				a, b := uniqueJobName("job-a"), uniqueJobName("job-b")
				if claimed, err := runs.Claim(ctx, config.JobRunClaim{Job: a, Occurrence: occurrence}, "n"); err != nil || !claimed {
					t.Fatalf("job A: claimed=%v err=%v", claimed, err)
				}
				if claimed, err := runs.Claim(ctx, config.JobRunClaim{Job: b, Occurrence: occurrence}, "n"); err != nil || !claimed {
					t.Fatalf("job B at the same instant was blocked by job A: claimed=%v err=%v", claimed, err)
				}
			})

			t.Run("concurrent claims elect one runner", func(t *testing.T) {
				newRuns := build(t)
				ctx := context.Background()
				job := uniqueJobName("race")

				const nodes = 8
				stores := make([]config.JobRunStore, nodes)
				for i := range stores {
					stores[i] = newRuns(t)
				}

				var (
					mu      sync.Mutex
					winners int
					errs    []error
					wg      sync.WaitGroup
					start   = make(chan struct{})
				)
				for i, runs := range stores {
					wg.Add(1)
					go func(i int, runs config.JobRunStore) {
						defer wg.Done()
						<-start
						claimed, err := runs.Claim(ctx, config.JobRunClaim{Job: job, Occurrence: occurrence}, fmt.Sprintf("node-%d", i))
						mu.Lock()
						defer mu.Unlock()
						switch {
						case err != nil:
							errs = append(errs, err)
						case claimed:
							winners++
						}
					}(i, runs)
				}
				close(start)
				wg.Wait()

				for _, err := range errs {
					t.Errorf("Claim errored during a contended race: %v", err)
				}
				if winners != 1 {
					t.Fatalf("%d nodes claimed the same occurrence, want exactly 1", winners)
				}
			})

			t.Run("last run reports the newest slot", func(t *testing.T) {
				newRuns := build(t)
				ctx := context.Background()
				runs := newRuns(t)
				job := uniqueJobName("last")

				if _, ok, err := runs.LastRun(ctx, job); err != nil || ok {
					t.Fatalf("LastRun before any claim: ok=%v err=%v; want false, nil", ok, err)
				}

				// Claimed out of order, so the answer cannot come from insertion
				// order.
				newest := occurrence.Add(2 * time.Minute)
				for _, slot := range []time.Time{occurrence.Add(time.Minute), newest, occurrence} {
					if _, err := runs.Claim(ctx, config.JobRunClaim{Job: job, Occurrence: slot}, "n"); err != nil {
						t.Fatalf("claim %v: %v", slot, err)
					}
				}
				got, ok, err := runs.LastRun(ctx, job)
				if err != nil || !ok {
					t.Fatalf("LastRun: ok=%v err=%v", ok, err)
				}
				if !got.Equal(newest) {
					t.Errorf("LastRun = %v, want %v", got, newest)
				}
			})

			t.Run("prune drops old claims and keeps recent ones", func(t *testing.T) {
				newRuns := build(t)
				ctx := context.Background()
				runs := newRuns(t)
				job := uniqueJobName("prune")

				old := occurrence.Add(-30 * 24 * time.Hour)
				if _, err := runs.Claim(ctx, config.JobRunClaim{Job: job, Occurrence: old}, "n"); err != nil {
					t.Fatal(err)
				}
				if _, err := runs.Claim(ctx, config.JobRunClaim{Job: job, Occurrence: occurrence}, "n"); err != nil {
					t.Fatal(err)
				}

				if _, err := runs.Prune(ctx, occurrence.Add(-time.Hour)); err != nil {
					t.Fatalf("Prune: %v", err)
				}

				// The recent claim must survive — pruning it would let its
				// occurrence be claimed a second time.
				if claimed, err := runs.Claim(ctx, config.JobRunClaim{Job: job, Occurrence: occurrence}, "n"); err != nil || claimed {
					t.Errorf("prune removed a live claim: claimed=%v err=%v", claimed, err)
				}
				// The old one is gone, so its slot is free again. That is
				// harmless: nothing will ever fire for a month-old occurrence.
				if claimed, err := runs.Claim(ctx, config.JobRunClaim{Job: job, Occurrence: old}, "n"); err != nil || !claimed {
					t.Errorf("prune did not remove the expired claim: claimed=%v err=%v", claimed, err)
				}
			})

			t.Run("rejects an unusable claim", func(t *testing.T) {
				newRuns := build(t)
				runs := newRuns(t)
				ctx := context.Background()

				if _, err := runs.Claim(ctx, config.JobRunClaim{Occurrence: occurrence}, "n"); err == nil {
					t.Error("Claim accepted a nameless job")
				}
				if _, err := runs.Claim(ctx, config.JobRunClaim{Job: "x"}, "n"); err == nil {
					t.Error("Claim accepted a zero occurrence")
				}
			})
		})
	}
}
