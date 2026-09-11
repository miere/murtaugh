package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/miere/murtaugh/internal/config"
)

// Follows database.backend so a token minted on one gateway resolves on whichever
// gateway the node's connection lands on.
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
