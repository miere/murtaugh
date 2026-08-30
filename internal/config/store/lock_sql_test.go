package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

// sqlLockerFactory builds fresh lockers that all contend on ONE lock row, which
// is how these tests stand in for separate nodes.
type sqlLockerFactory func(t *testing.T, ttl time.Duration) config.Locker

// sqliteLockerFactory runs the SQL lease against SQLite.
//
// SQLite does not USE this lease in production — it has a strictly better local
// lock in flock — but running the same code against it is what makes the
// compare-and-swap, the epoch ordering and the expiry takeover testable without
// a database, on every machine and every CI run. The Postgres factory below
// then proves the same behaviours over the real dialect.
func sqliteLockerFactory(t *testing.T) sqlLockerFactory {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lock.db")
	return func(t *testing.T, ttl time.Duration) config.Locker {
		t.Helper()
		db, err := sql.Open("sqlite", sqliteDSN(path))
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		db.SetMaxOpenConns(1)
		locker, err := openSQLLocker(context.Background(), db, sqliteDialect{}, testIdentity(), ttl, true)
		if err != nil {
			t.Fatalf("openSQLLocker: %v", err)
		}
		t.Cleanup(func() { _ = locker.Close() })
		return locker
	}
}

// postgresLockerFactory runs the lease against a real Postgres, skipping when
// MURTAUGH_TEST_POSTGRES_DSN is unset — the same bargain the config-store
// integration tests strike, and the variable CI already sets.
func postgresLockerFactory(t *testing.T) sqlLockerFactory {
	t.Helper()
	dsn := postgresTestDSN(t)

	// A per-run lock key keeps repeated runs against a shared database from
	// inheriting a live lease, which would make every acquisition here fail.
	identity := config.LockIdentity{TeamID: "T0SQLTEST", AppID: uniqueAppID()}
	return func(t *testing.T, ttl time.Duration) config.Locker {
		t.Helper()
		locker, err := openPostgresLocker(context.Background(), dsn, identity, ttl)
		if err != nil {
			t.Fatalf("openPostgresLocker: %v", err)
		}
		t.Cleanup(func() { _ = locker.Close() })
		return locker
	}
}

// TestSQLLockerBehaviours runs the whole lease contract against every SQL
// dialect that implements it. Postgres is the backend that ships this lease;
// SQLite is here so the logic is covered even where no database is available.
func TestSQLLockerBehaviours(t *testing.T) {
	for name, build := range map[string]func(*testing.T) sqlLockerFactory{
		"sqlite":   sqliteLockerFactory,
		"postgres": postgresLockerFactory,
	} {
		t.Run(name, func(t *testing.T) {
			t.Run("excludes a second node", func(t *testing.T) {
				newLocker := build(t)
				ctx := context.Background()

				leader := newLocker(t, time.Minute)
				lease, ok, err := leader.Acquire(ctx)
				if err != nil || !ok {
					t.Fatalf("leader Acquire: ok=%v err=%v", ok, err)
				}
				if !lease.Expires() {
					t.Error("a SQL lease must carry a deadline; the caller would skip renewal")
				}

				standby := newLocker(t, time.Minute)
				if _, ok, err := standby.Acquire(ctx); err != nil || ok {
					t.Fatalf("standby Acquire: ok=%v err=%v; want refused with no error", ok, err)
				}
			})

			t.Run("takeover invalidates the old holder", func(t *testing.T) {
				newLocker := build(t)
				ctx := context.Background()

				old := newLocker(t, time.Second)
				lease, ok, err := old.Acquire(ctx)
				if err != nil || !ok {
					t.Fatalf("Acquire: ok=%v err=%v", ok, err)
				}

				// The challenger takes a long lease deliberately: only the
				// ORIGINAL holder's TTL needs to be short for expiry to be
				// reachable. A one-second lease here would race the several
				// round-trips below against its own clock, and a loaded runner
				// would fail on a lease that legitimately lapsed.
				time.Sleep(1500 * time.Millisecond)
				challenger := newLocker(t, time.Minute)
				taken, ok, err := challenger.Acquire(ctx)
				if err != nil || !ok {
					t.Fatalf("challenger Acquire after expiry: ok=%v err=%v", ok, err)
				}
				if taken.Epoch <= lease.Epoch {
					t.Errorf("epoch did not advance: old=%d new=%d", lease.Epoch, taken.Epoch)
				}

				// The displaced node must learn it lost, from both paths.
				if _, ok, err := old.Renew(ctx, lease); err != nil || ok {
					t.Errorf("old holder's Renew: ok=%v err=%v; want a clean loss", ok, err)
				}
				if ok, err := old.Verify(ctx, lease); err != nil || ok {
					t.Errorf("old holder's Verify: ok=%v err=%v; want not held", ok, err)
				}
				if ok, err := challenger.Verify(ctx, taken); err != nil || !ok {
					t.Errorf("new holder's Verify: ok=%v err=%v; want still held", ok, err)
				}
			})

			t.Run("renewal holds off challengers", func(t *testing.T) {
				newLocker := build(t)
				ctx := context.Background()

				leader := newLocker(t, 2*time.Second)
				lease, ok, err := leader.Acquire(ctx)
				if err != nil || !ok {
					t.Fatalf("Acquire: ok=%v err=%v", ok, err)
				}
				challenger := newLocker(t, 2*time.Second)

				for i := 0; i < 5; i++ {
					time.Sleep(600 * time.Millisecond)
					renewed, ok, err := leader.Renew(ctx, lease)
					if err != nil || !ok {
						t.Fatalf("renew %d: ok=%v err=%v; a healthy leader lost its lease", i, ok, err)
					}
					lease = renewed
					if _, ok, err := challenger.Acquire(ctx); err != nil || ok {
						t.Fatalf("challenger took the lock from a renewing leader at %d: ok=%v err=%v", i, ok, err)
					}
				}
			})

			t.Run("release hands over immediately", func(t *testing.T) {
				newLocker := build(t)
				ctx := context.Background()

				leader := newLocker(t, time.Minute)
				lease, ok, err := leader.Acquire(ctx)
				if err != nil || !ok {
					t.Fatalf("Acquire: ok=%v err=%v", ok, err)
				}
				standby := newLocker(t, time.Minute)
				if _, ok, _ := standby.Acquire(ctx); ok {
					t.Fatal("standby acquired while the lease was live")
				}

				if err := leader.Release(ctx, lease); err != nil {
					t.Fatalf("Release: %v", err)
				}
				promoted, ok, err := standby.Acquire(ctx)
				if err != nil || !ok {
					t.Fatalf("standby Acquire after release: ok=%v err=%v", ok, err)
				}
				// The epoch must keep counting across a clean handover: flagging
				// the row rather than deleting it is what preserves the ordering.
				if promoted.Epoch != lease.Epoch+1 {
					t.Errorf("epoch = %d after a clean handover, want %d", promoted.Epoch, lease.Epoch+1)
				}
			})

			t.Run("stale release does not free a successor", func(t *testing.T) {
				newLocker := build(t)
				ctx := context.Background()

				old := newLocker(t, time.Second)
				lease, ok, err := old.Acquire(ctx)
				if err != nil || !ok {
					t.Fatalf("Acquire: ok=%v err=%v", ok, err)
				}
				time.Sleep(1500 * time.Millisecond)

				challenger := newLocker(t, time.Minute)
				taken, ok, err := challenger.Acquire(ctx)
				if err != nil || !ok {
					t.Fatalf("challenger Acquire: ok=%v err=%v", ok, err)
				}

				// The demoted node finally reaches its shutdown path.
				if err := old.Release(ctx, lease); err != nil {
					t.Errorf("stale Release should be harmless, got: %v", err)
				}
				if ok, err := challenger.Verify(ctx, taken); err != nil || !ok {
					t.Errorf("a stale Release destroyed the successor's lock: ok=%v err=%v", ok, err)
				}
			})

			t.Run("concurrent acquire elects exactly one", func(t *testing.T) {
				newLocker := build(t)
				ctx := context.Background()

				const nodes = 8
				lockers := make([]config.Locker, nodes)
				for i := range lockers {
					lockers[i] = newLocker(t, time.Minute)
				}

				var (
					mu      sync.Mutex
					winners []config.Lease
					errs    []error
					wg      sync.WaitGroup
					start   = make(chan struct{})
				)
				for _, locker := range lockers {
					wg.Add(1)
					go func(l config.Locker) {
						defer wg.Done()
						<-start
						lease, ok, err := l.Acquire(ctx)
						mu.Lock()
						defer mu.Unlock()
						switch {
						case err != nil:
							errs = append(errs, err)
						case ok:
							winners = append(winners, lease)
						}
					}(locker)
				}
				close(start)
				wg.Wait()

				for _, err := range errs {
					t.Errorf("Acquire errored during a contended race: %v", err)
				}
				if len(winners) != 1 {
					t.Fatalf("%d nodes won the election, want exactly 1: %+v", len(winners), winners)
				}
			})

			t.Run("judges by the recorded lease", func(t *testing.T) {
				newLocker := build(t)
				ctx := context.Background()

				// The incumbent takes a long lease.
				leader := newLocker(t, time.Minute)
				if _, ok, err := leader.Acquire(ctx); err != nil || !ok {
					t.Fatalf("leader Acquire: ok=%v err=%v", ok, err)
				}
				// A challenger configured with a much shorter one must back off:
				// evicting a leader that is renewing correctly on its own terms
				// would make one misconfigured node destabilise the cluster.
				impatient := newLocker(t, time.Second)
				time.Sleep(1500 * time.Millisecond)
				if _, ok, err := impatient.Acquire(ctx); err != nil || ok {
					t.Fatalf("a short-TTL challenger evicted a healthy leader: ok=%v err=%v", ok, err)
				}
			})

			t.Run("verify rejects a foreign lease", func(t *testing.T) {
				newLocker := build(t)
				ctx := context.Background()

				leader := newLocker(t, time.Minute)
				lease, ok, err := leader.Acquire(ctx)
				if err != nil || !ok {
					t.Fatalf("Acquire: ok=%v err=%v", ok, err)
				}
				stale := lease
				stale.Epoch = lease.Epoch - 1
				if ok, err := leader.Verify(ctx, stale); err != nil || ok {
					t.Errorf("Verify accepted an older epoch: ok=%v err=%v", ok, err)
				}
				if ok, err := leader.Verify(ctx, config.Lease{}); err != nil || ok {
					t.Errorf("Verify accepted a zero lease: ok=%v err=%v", ok, err)
				}
			})

			t.Run("ttl defaults rather than expiring instantly", func(t *testing.T) {
				newLocker := build(t)
				// A zero TTL on this backend would mean "already expired", which
				// would produce continuous takeovers rather than an election.
				if got := newLocker(t, 0).TTL(); got != DefaultLeaseTTL {
					t.Errorf("TTL() = %v, want the default %v", got, DefaultLeaseTTL)
				}
			})
		})
	}
}

// TestOpenLockerSelectsPostgres checks the seam dispatches on database.backend,
// which is what keeps the lock in the same store as the config it came from.
func TestOpenLockerSelectsPostgres(t *testing.T) {
	dsn := postgresTestDSN(t)
	locker, err := OpenLocker(context.Background(),
		config.DatabaseConfig{Backend: config.BackendPostgres, Postgres: config.PostgresConfig{DSN: dsn}},
		config.LockIdentity{TeamID: "T0DISPATCH", AppID: uniqueAppID()}, 0)
	if err != nil {
		t.Fatalf("OpenLocker(postgres): %v", err)
	}
	defer locker.Close()

	if got := locker.Backend(); got != config.BackendPostgres {
		t.Errorf("Backend() = %q, want %q", got, config.BackendPostgres)
	}
	if got := locker.TTL(); got != DefaultLeaseTTL {
		t.Errorf("TTL() = %v, want %v", got, DefaultLeaseTTL)
	}
}

// TestPostgresLockerRequiresADSN guards the one way this backend can be
// misconfigured into silently doing nothing.
func TestPostgresLockerRequiresADSN(t *testing.T) {
	if _, err := openPostgresLocker(context.Background(), "", testIdentity(), 0); err == nil {
		t.Fatal("openPostgresLocker accepted an empty DSN")
	}
}
