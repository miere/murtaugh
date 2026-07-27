package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // pure-Go Postgres driver (registers "pgx")

	"github.com/miere/murtaugh/internal/config"
)

// postgresDialect targets Postgres via pgx's database/sql driver. Bodies use a
// native JSONB column (the body param is cast with ::jsonb); timestamps are
// TIMESTAMPTZ.
type postgresDialect struct{}

func (postgresDialect) Name() string             { return "postgres" }
func (postgresDialect) Placeholder(n int) string { return fmt.Sprintf("$%d", n) }
func (postgresDialect) JSONValue(n int) string   { return fmt.Sprintf("$%d::jsonb", n) }
func (postgresDialect) JSONType() string         { return "JSONB" }
func (postgresDialect) TimestampType() string    { return "TIMESTAMPTZ" }
func (postgresDialect) Now() string              { return "now()" }

// openPostgres connects to the Postgres config store described by dsn and brings
// its schema up to date. The DSN is a libpq/pgx connection string (URL or
// key=value form), supplied from .env via ${VAR} — never a literal in YAML.
func openPostgres(ctx context.Context, dsn string) (config.Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("database.postgres.dsn is required for the postgres backend")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	return newStore(ctx, db, postgresDialect{})
}
