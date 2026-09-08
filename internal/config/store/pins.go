package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/miere/murtaugh/internal/config"
)

// OpenConversationPins returns the delegated-conversation store for the
// configured backend.
//
// It follows `database.backend` for the reason the leader lock, the run claim
// and the node credential store do: whichever gateway is leading has to read
// the same pin the previous leader wrote, and the store their shared
// configuration came from is the only place every gateway already agrees on. A
// pin written somewhere the next leader does not look is not a pin — it is a
// conversation that silently moves machines the first time leadership changes,
// which is precisely the failure #170 asks this to prevent.
//
// On SQLite the store is local, and so are the pins, which is right: on SQLite
// there is only ever one gateway.
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
