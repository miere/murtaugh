package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations lists the config-store schema versions in order. Each entry is the
// set of DDL statements that bring the schema from version i to version i+1.
// The statements are dialect-parameterized (JSON/timestamp column types) so the
// same list serves SQLite and Postgres. Add a new inner slice to evolve the
// schema; never edit a shipped one.
func migrations(d Dialect) [][]string {
	return [][]string{
		// v1 — the document store: collection entities keyed by (section, name),
		// single-valued blocks keyed by key.
		{
			fmt.Sprintf(`CREATE TABLE IF NOT EXISTS config_items (
				section    TEXT NOT NULL,
				name       TEXT NOT NULL,
				body       %s   NOT NULL,
				updated_at %s   NOT NULL,
				PRIMARY KEY (section, name)
			)`, d.JSONType(), d.TimestampType()),
			fmt.Sprintf(`CREATE TABLE IF NOT EXISTS config_singletons (
				key        TEXT PRIMARY KEY,
				body       %s   NOT NULL,
				updated_at %s   NOT NULL
			)`, d.JSONType(), d.TimestampType()),
		},
		// v2 — the leader lease. One row per Slack app: exactly one gateway may
		// hold a live socket for it, and this row is what decides which.
		//
		// acquired_at is written with the DATABASE's clock and compared against
		// it, never against a node's; lease_seconds travels with the row so a
		// challenger judges the incumbent by the terms the incumbent took the
		// lock on. fence is the compare-and-swap token, replaced on every
		// acquisition, which is how a displaced holder discovers it has lost.
		{
			fmt.Sprintf(`CREATE TABLE IF NOT EXISTS leader_locks (
				lock_key      TEXT PRIMARY KEY,
				owner         TEXT    NOT NULL,
				fence         TEXT    NOT NULL,
				epoch         BIGINT  NOT NULL,
				acquired_at   %s      NOT NULL,
				lease_seconds BIGINT  NOT NULL,
				released      INTEGER NOT NULL,
				team_id       TEXT    NOT NULL,
				app_id        TEXT    NOT NULL
			)`, d.TimestampType()),
		},
		// v3 — scheduled-run claims. One row per (job, occurrence): the primary
		// key IS the mutual exclusion, so an insert that affects a row is a
		// won claim and one that affects none is a job another node — or an
		// earlier incarnation of this one — has already run.
		//
		// occurrence is TEXT in every dialect on purpose. It is an identity
		// rather than a moment: its only job is to be byte-identical across
		// nodes so the key collides, and leaving it to each driver's timestamp
		// handling would risk two representations of one instant both winning.
		{
			fmt.Sprintf(`CREATE TABLE IF NOT EXISTS job_runs (
				job        TEXT NOT NULL,
				occurrence TEXT NOT NULL,
				node       TEXT NOT NULL,
				claimed_at %s   NOT NULL,
				PRIMARY KEY (job, occurrence)
			)`, d.TimestampType()),
		},
	}
}

// runMigrations brings db up to the latest schema version. It records applied
// versions in schema_migrations and applies each pending version's statements
// in its own transaction, so a failure rolls that version back cleanly. It is
// idempotent: an already-current database is left untouched.
func runMigrations(ctx context.Context, db *sql.DB, d Dialect) error {
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at %s NOT NULL)`,
		d.TimestampType())); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	var current int
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	all := migrations(d)
	for i := current; i < len(all); i++ {
		version := i + 1
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", version, err)
		}
		for _, stmt := range all[i] {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("apply migration %d: %w", version, err)
			}
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO schema_migrations (version, applied_at) VALUES (%s, %s)`, d.Placeholder(1), d.Now()),
			version); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("stamp migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", version, err)
		}
	}
	return nil
}
