package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/miere/murtaugh/internal/config"
)

// sqlLocker is the leader lease over a relational config store.
//
// It is the Postgres backend's answer to the same question Firestore answers,
// and it reaches it the same way: the store cannot observe a holder's death, so
// liveness is a lease the holder renews, and every timestamp in that judgement
// comes from the database rather than from any node.
//
// # The fence token
//
// Firestore gets its compare-and-swap from document write preconditions. SQL
// has no equivalent, so this carries an explicit one: each acquisition mints a
// fresh fence (a UUID) and writes it into the row, and every subsequent
// operation this node performs is conditioned on `fence = <ours>`. A takeover
// replaces the fence, so the previous holder's next renewal matches zero rows
// and it learns it has lost — from the write it was going to make anyway,
// exactly as the Firestore path does.
//
// Conditioning on the fence rather than on the owner string matters: a node
// restarting on the same host with a recycled PID would present an identical
// owner, and would otherwise mistake its successor's lock for its own.
//
// # Why it is written against Dialect
//
// The same code serves Postgres and SQLite. Postgres is the point — SQLite has
// a strictly better local lock in flock and uses that instead. But being able
// to run this logic against SQLite is what makes the compare-and-swap, the
// epoch ordering, and the expiry takeover testable without standing up a
// database, which is the difference between a lock that is believed to work and
// one that has been watched working.
type sqlLocker struct {
	db       *sql.DB
	d        Dialect
	identity config.LockIdentity
	ttl      time.Duration
	// ownsDB records whether Close should shut the handle down. A locker that
	// opened its own connection owns it; one sharing the config store's must
	// not close it out from under the store.
	ownsDB bool

	mu sync.Mutex
	// fence is the token proving the current lease is ours. Empty when not
	// holding. It is the whole of this locker's mutable state: a node holds at
	// most one lease at a time.
	fence string
}

// openSQLLocker prepares the lease for identity over an existing handle.
func openSQLLocker(ctx context.Context, db *sql.DB, d Dialect, identity config.LockIdentity, ttl time.Duration, ownsDB bool) (config.Locker, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	// The locker runs the store's migrations rather than assuming the config
	// store already did. It may be opened on its own connection, before or
	// without the store, and an absent table would otherwise fail at the first
	// acquisition instead of at startup.
	if err := runMigrations(ctx, db, d); err != nil {
		return nil, fmt.Errorf("migrate leader lock schema: %w", err)
	}
	return &sqlLocker{db: db, d: d, identity: identity, ttl: ttl, ownsDB: ownsDB}, nil
}

// openPostgresLocker opens a dedicated Postgres connection for the lease.
//
// It is deliberately its own connection rather than the config store's. The
// renewal loop must keep working while the store is idle or busy, and a lease
// that shares a saturated pool with config reads can be starved into a spurious
// demotion — which costs a failover, not just a slow query.
func openPostgresLocker(ctx context.Context, dsn string, identity config.LockIdentity, ttl time.Duration) (config.Locker, error) {
	if dsn == "" {
		return nil, errors.New("database.postgres.dsn is required for the postgres backend")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres for the leader lock: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect postgres for the leader lock: %w", err)
	}
	// A handful of connections is ample for one lease, and capping it keeps the
	// lock from competing with the application for the server's slots.
	db.SetMaxOpenConns(2)

	locker, err := openSQLLocker(ctx, db, postgresDialect{}, identity, ttl, true)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return locker, nil
}

func (l *sqlLocker) Backend() string { return l.d.Name() }

func (l *sqlLocker) TTL() time.Duration { return l.ttl }

func (l *sqlLocker) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.ownsDB {
		return nil
	}
	return l.db.Close()
}

// ph is shorthand for the dialect's n-th bind placeholder.
func (l *sqlLocker) ph(n int) string { return l.d.Placeholder(n) }

// expired is the dialect's "this lease has lapsed" expression over the row's
// own columns.
func (l *sqlLocker) expired() string { return l.d.LeaseExpired("acquired_at", "lease_seconds") }

// Acquire takes the lock when it is free, released, or its lease has lapsed.
//
// Two statements, either of which is a complete compare-and-swap:
//
//   - INSERT ... ON CONFLICT DO NOTHING creates the row, and does nothing if
//     another node created it first.
//   - UPDATE ... WHERE released OR expired takes over a lapsed lease, and
//     matches nothing if another node renewed or took it in between.
//
// Losing either race yields ok=false and a nil error, because losing a race is
// the ordinary standby outcome and not a failure.
func (l *sqlLocker) Acquire(ctx context.Context) (config.Lease, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	fence := uuid.NewString()
	owner := config.OwnerID()
	key := l.identity.Key()
	seconds := int64(l.ttl / time.Second)

	insert := fmt.Sprintf(
		`INSERT INTO leader_locks (lock_key, owner, fence, epoch, acquired_at, lease_seconds, released, team_id, app_id)
		 VALUES (%s, %s, %s, 1, %s, %s, 0, %s, %s)
		 ON CONFLICT (lock_key) DO NOTHING`,
		l.ph(1), l.ph(2), l.ph(3), l.d.Now(), l.ph(4), l.ph(5), l.ph(6))
	res, err := l.db.ExecContext(ctx, insert, key, owner, fence, seconds, l.identity.TeamID, l.identity.AppID)
	if err != nil {
		return config.Lease{}, false, fmt.Errorf("acquire leader lock: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 1 {
		return l.finishAcquire(ctx, fence)
	}

	// The row exists. Take it only if it is free — released, or lapsed by the
	// server's own reckoning. The epoch advances in SQL so takeovers stay
	// totally ordered without a read-then-write that another node could
	// interleave with.
	takeover := fmt.Sprintf(
		`UPDATE leader_locks
		    SET owner = %s, fence = %s, epoch = epoch + 1, acquired_at = %s,
		        lease_seconds = %s, released = 0
		  WHERE lock_key = %s AND (released = 1 OR %s)`,
		l.ph(1), l.ph(2), l.d.Now(), l.ph(3), l.ph(4), l.expired())
	res, err = l.db.ExecContext(ctx, takeover, owner, fence, seconds, key)
	if err != nil {
		return config.Lease{}, false, fmt.Errorf("acquire leader lock: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return config.Lease{}, false, fmt.Errorf("acquire leader lock: %w", err)
	}
	if n == 0 {
		return config.Lease{}, false, nil // validly held by another node
	}
	return l.finishAcquire(ctx, fence)
}

// finishAcquire reads back the row we just claimed, so the epoch and the
// deadline come from the database rather than from this node's guess about what
// it wrote.
func (l *sqlLocker) finishAcquire(ctx context.Context, fence string) (config.Lease, bool, error) {
	lease, ok, err := l.readLease(ctx, fence)
	if err != nil {
		return config.Lease{}, false, fmt.Errorf("acquire leader lock: %w", err)
	}
	if !ok {
		// Claimed and lost between the write and the read — a lapsed-lease race
		// against a node with a much shorter TTL. Rare, and correctly reported
		// as "did not get it".
		return config.Lease{}, false, nil
	}
	l.fence = fence
	return lease, true, nil
}

// readLease reads the row iff it is still held under fence.
func (l *sqlLocker) readLease(ctx context.Context, fence string) (config.Lease, bool, error) {
	query := fmt.Sprintf(
		`SELECT owner, epoch, acquired_at, lease_seconds
		   FROM leader_locks
		  WHERE lock_key = %s AND fence = %s AND released = 0 AND NOT (%s)`,
		l.ph(1), l.ph(2), l.expired())

	var (
		owner   string
		epoch   int64
		raw     any
		seconds int64
	)
	err := l.db.QueryRowContext(ctx, query, l.identity.Key(), fence).Scan(&owner, &epoch, &raw, &seconds)
	if errors.Is(err, sql.ErrNoRows) {
		return config.Lease{}, false, nil
	}
	if err != nil {
		return config.Lease{}, false, err
	}
	acquired, err := scanTimestamp(raw)
	if err != nil {
		return config.Lease{}, false, err
	}
	return config.Lease{
		Key:   l.identity.Key(),
		Owner: owner,
		Epoch: epoch,
		// Server-stamped acquisition plus the lease length: the deadline is
		// expressed in the database's timeline, which is the only one it means
		// anything in. The caller uses it to schedule renewals, not to decide
		// whether it still leads — that goes through Verify.
		ExpiresAt: acquired.Add(time.Duration(seconds) * time.Second),
	}, true, nil
}

// scanTimestamp converts a driver's timestamp representation to a time.Time.
//
// The two dialects disagree here and neither is wrong: Postgres has a real
// TIMESTAMPTZ and pgx hands back a time.Time, while SQLite has no timestamp
// type at all, so the column is TEXT and the driver hands back a string. Both
// values were produced by the database itself, so the only job is decoding.
//
// This feeds the lease deadline the caller uses to schedule renewals, never the
// expiry decision — that is made in SQL, by the server, and never round-trips
// through this function.
func scanTimestamp(raw any) (time.Time, error) {
	switch v := raw.(type) {
	case time.Time:
		return v, nil
	case []byte:
		return parseSQLiteTimestamp(string(v))
	case string:
		return parseSQLiteTimestamp(v)
	default:
		return time.Time{}, fmt.Errorf("unsupported timestamp type %T in the leader lock row", raw)
	}
}

// sqliteTimestampLayouts are the forms SQLite's CURRENT_TIMESTAMP and datetime()
// produce, most specific first.
var sqliteTimestampLayouts = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	time.RFC3339Nano,
	time.RFC3339,
}

// parseSQLiteTimestamp decodes SQLite's textual timestamp. The values are UTC
// (CURRENT_TIMESTAMP always is), so a layout without a zone is read as UTC
// rather than as this machine's local time — which would shift the deadline by
// the local offset and make renewals look overdue or premature.
func parseSQLiteTimestamp(s string) (time.Time, error) {
	for _, layout := range sqliteTimestampLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp %q in the leader lock row", s)
}

// Renew extends a held lease. The fence condition is the fence: if a takeover
// has happened, this matches no rows and the caller learns it has lost.
func (l *sqlLocker) Renew(ctx context.Context, lease config.Lease) (config.Lease, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !lease.Held() || lease.Key != l.identity.Key() || l.fence == "" {
		return config.Lease{}, false, nil
	}

	stmt := fmt.Sprintf(
		`UPDATE leader_locks SET acquired_at = %s
		  WHERE lock_key = %s AND fence = %s AND released = 0 AND NOT (%s)`,
		l.d.Now(), l.ph(1), l.ph(2), l.expired())
	res, err := l.db.ExecContext(ctx, stmt, l.identity.Key(), l.fence)
	if err != nil {
		// A transport failure is "cannot tell", not "lost". The caller keeps
		// the lease; its own stand-down deadline decides when holding on stops
		// being defensible.
		return config.Lease{}, false, fmt.Errorf("renew leader lock: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return config.Lease{}, false, fmt.Errorf("renew leader lock: %w", err)
	}
	if n == 0 {
		return config.Lease{}, false, nil
	}

	renewed, ok, err := l.readLease(ctx, l.fence)
	if err != nil {
		return config.Lease{}, false, fmt.Errorf("renew leader lock: %w", err)
	}
	return renewed, ok, nil
}

// Verify re-reads the lock and reports whether this lease is still the live
// claim. It is the synchronous check the outbound gate makes when local
// timekeeping cannot be trusted — see the Firestore locker's Verify for why
// elapsed time on this node is not an acceptable substitute.
func (l *sqlLocker) Verify(ctx context.Context, lease config.Lease) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !lease.Held() || lease.Key != l.identity.Key() || l.fence == "" {
		return false, nil
	}
	current, ok, err := l.readLease(ctx, l.fence)
	if err != nil {
		return false, fmt.Errorf("verify leader lock: %w", err)
	}
	return ok && current.Epoch == lease.Epoch, nil
}

// Release marks the lock free so a standby promotes at once rather than waiting
// out the TTL.
//
// It flags the row rather than deleting it, so the epoch survives the handover.
// Deleting would restart the count at 1 on the next acquisition, destroying the
// ordering the epoch exists to provide. The fence condition keeps a demoted
// node from freeing a successor's lock.
func (l *sqlLocker) Release(ctx context.Context, lease config.Lease) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !lease.Held() || lease.Key != l.identity.Key() || l.fence == "" {
		return nil
	}
	fence := l.fence
	l.fence = ""

	stmt := fmt.Sprintf(
		`UPDATE leader_locks SET released = 1 WHERE lock_key = %s AND fence = %s`,
		l.ph(1), l.ph(2))
	if _, err := l.db.ExecContext(ctx, stmt, l.identity.Key(), fence); err != nil {
		return fmt.Errorf("release leader lock: %w", err)
	}
	return nil
}
