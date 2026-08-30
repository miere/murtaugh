package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/miere/murtaugh/internal/config"
)

// OpenJobRuns returns the scheduled-run claim store for the configured backend.
//
// It follows `database.backend` for the same reason the leader lock does: nodes
// taking turns at serving one Slack app must agree on where this state lives,
// and the store their shared configuration came from is the only place they are
// already guaranteed to agree on. A claim written somewhere the next leader
// does not look is not a claim.
//
// On SQLite the store is local, and so are the claims — which is exactly right,
// because on SQLite there is only ever one machine.
func OpenJobRuns(ctx context.Context, dbc config.DatabaseConfig, configDir, baseName string) (config.JobRunStore, error) {
	switch backend := dbc.EffectiveBackend(); backend {
	case config.BackendSQLite:
		db, err := openSQLiteDB(dbc.EffectiveSQLitePath(configDir, baseName))
		if err != nil {
			return nil, err
		}
		runs, err := openSQLJobRuns(ctx, db, sqliteDialect{}, true)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		return runs, nil
	case config.BackendPostgres:
		return openPostgresJobRuns(ctx, strings.TrimSpace(dbc.Postgres.DSN))
	case config.BackendFirestore:
		return openFirestoreJobRuns(ctx, dbc.Firestore)
	default:
		return nil, fmt.Errorf("unknown database backend %q (want sqlite, postgres, or firestore)", dbc.Backend)
	}
}
