package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/miere/murtaugh/internal/config"
)

// OpenNodeTokens returns the issued-credential store for the configured
// backend.
//
// It follows `database.backend` for the same reason the leader lock and the
// run-claim store do: whichever gateway a node's connection lands on has to
// resolve that node's token, and the store their shared configuration came from
// is the only place every gateway already agrees on. A credential minted
// somewhere the verifying gateway does not look is not a credential.
//
// On SQLite the store is local, and so are the credentials — which is right,
// because on SQLite there is only ever one gateway.
func OpenNodeTokens(ctx context.Context, dbc config.DatabaseConfig, configDir, baseName string) (config.NodeTokenStore, error) {
	switch backend := dbc.EffectiveBackend(); backend {
	case config.BackendSQLite:
		db, err := openSQLiteDB(dbc.EffectiveSQLitePath(configDir, baseName))
		if err != nil {
			return nil, err
		}
		tokens, err := openSQLNodeTokens(ctx, db, sqliteDialect{}, true)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		return tokens, nil
	case config.BackendPostgres:
		return openPostgresNodeTokens(ctx, strings.TrimSpace(dbc.Postgres.DSN))
	case config.BackendFirestore:
		return openFirestoreNodeTokens(ctx, dbc.Firestore)
	default:
		return nil, fmt.Errorf("unknown database backend %q (want sqlite, postgres, or firestore)", dbc.Backend)
	}
}
