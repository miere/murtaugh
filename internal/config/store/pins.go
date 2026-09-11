package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/miere/murtaugh/internal/config"
)

// Follows database.backend so the next leader reads the pin the last one wrote; otherwise
// a conversation silently moves machines on the first leadership change.
func OpenConversationPins(ctx context.Context, dbc config.DatabaseConfig, configDir, baseName string) (config.ConversationPinStore, error) {
	switch backend := dbc.EffectiveBackend(); backend {
	case config.BackendSQLite:
		db, err := openSQLiteDB(dbc.EffectiveSQLitePath(configDir, baseName))
		if err != nil {
			return nil, err
		}
		pins, err := openSQLConversationPins(ctx, db, sqliteDialect{}, true)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		return pins, nil
	case config.BackendPostgres:
		return openPostgresConversationPins(ctx, strings.TrimSpace(dbc.Postgres.DSN))
	case config.BackendFirestore:
		return openFirestoreConversationPins(ctx, dbc.Firestore)
	default:
		return nil, fmt.Errorf("unknown database backend %q (want sqlite, postgres, or firestore)", dbc.Backend)
	}
}
