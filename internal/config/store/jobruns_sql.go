package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

// sqlJobRuns records scheduled-run claims in a relational store, serving both
// SQLite and Postgres through the Dialect seam.
//
// The claim is one INSERT. `ON CONFLICT DO NOTHING` on the (job, occurrence)
// primary key is the whole of the mutual exclusion: whichever node's insert
// affects a row won the slot, and every other node — now, or after a restart
// that rebuilt its scheduler — affects none and stands down. There is no read
// before the write, so there is no window between checking and claiming.
type sqlJobRuns struct {
	db     *sql.DB
	d      Dialect
	ownsDB bool
}

// openSQLJobRuns prepares the claim store over an existing handle.
func openSQLJobRuns(ctx context.Context, db *sql.DB, d Dialect, ownsDB bool) (config.JobRunStore, error) {
	if err := runMigrations(ctx, db, d); err != nil {
		return nil, fmt.Errorf("migrate job-run schema: %w", err)
	}
	return &sqlJobRuns{db: db, d: d, ownsDB: ownsDB}, nil
}

// openPostgresJobRuns opens a dedicated Postgres connection for run claims.
func openPostgresJobRuns(ctx context.Context, dsn string) (config.JobRunStore, error) {
	if dsn == "" {
		return nil, errors.New("database.postgres.dsn is required for the postgres backend")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres for job runs: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect postgres for job runs: %w", err)
	}
	db.SetMaxOpenConns(2)

	runs, err := openSQLJobRuns(ctx, db, postgresDialect{}, true)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return runs, nil
}

func (s *sqlJobRuns) Close() error {
	if !s.ownsDB {
		return nil
	}
	return s.db.Close()
}

// Claim inserts the occurrence, reporting whether this caller took it.
//
// The occurrence is stored as a UTC string in a fixed layout rather than as a
// native timestamp. It is an identity, not a moment: its only job is to be
// byte-identical across nodes so the primary key collides. Leaving it to each
// driver's timestamp handling would risk two nodes writing the same instant in
// two representations and both "winning".
func (s *sqlJobRuns) Claim(ctx context.Context, claim config.JobRunClaim, node string) (bool, error) {
	if err := claim.Validate(); err != nil {
		return false, err
	}
	stmt := fmt.Sprintf(
		`INSERT INTO job_runs (job, occurrence, node, claimed_at) VALUES (%s, %s, %s, %s)
		 ON CONFLICT (job, occurrence) DO NOTHING`,
		s.d.Placeholder(1), s.d.Placeholder(2), s.d.Placeholder(3), s.d.Now())
	res, err := s.db.ExecContext(ctx, stmt, claim.Job, occurrenceKey(claim.Occurrence), node)
	if err != nil {
		return false, fmt.Errorf("claim run of %q: %w", claim.Job, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// A driver that cannot report affected rows leaves us unable to tell
		// whether we won. Not running is the safe reading: a skipped run is
		// recoverable, a double run may not be.
		return false, fmt.Errorf("claim run of %q: %w", claim.Job, err)
	}
	return n > 0, nil
}

func (s *sqlJobRuns) LastRun(ctx context.Context, job string) (time.Time, bool, error) {
	stmt := fmt.Sprintf(`SELECT MAX(occurrence) FROM job_runs WHERE job = %s`, s.d.Placeholder(1))
	var raw sql.NullString
	if err := s.db.QueryRowContext(ctx, stmt, job).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("last run of %q: %w", job, err)
	}
	if !raw.Valid || raw.String == "" {
		return time.Time{}, false, nil
	}
	at, err := time.Parse(occurrenceLayout, raw.String)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("last run of %q: unrecognised occurrence %q", job, raw.String)
	}
	return at.UTC(), true, nil
}

func (s *sqlJobRuns) Prune(ctx context.Context, before time.Time) (int64, error) {
	stmt := fmt.Sprintf(`DELETE FROM job_runs WHERE occurrence < %s`, s.d.Placeholder(1))
	res, err := s.db.ExecContext(ctx, stmt, occurrenceKey(before))
	if err != nil {
		return 0, fmt.Errorf("prune job runs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// occurrenceLayout is the fixed textual form of an occurrence key. Sortable
// (so MAX and range deletes work as ordering, not just equality) and identical
// on every node.
const occurrenceLayout = "2006-01-02T15:04:05.000Z"

// occurrenceKey renders an occurrence as its storage key.
func occurrenceKey(at time.Time) string { return at.UTC().Format(occurrenceLayout) }
