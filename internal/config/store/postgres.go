package store

import (
	"context"
	"errors"

	"github.com/miere/murtaugh/internal/config"
)

// openPostgres is a placeholder until the Postgres backend lands (WS4): the pgx
// driver + postgresDialect are added there. Selecting the postgres backend
// before then fails with a clear message rather than a nil dereference.
func openPostgres(_ context.Context, _ string) (config.Store, error) {
	return nil, errors.New("postgres backend is not yet available in this build")
}
