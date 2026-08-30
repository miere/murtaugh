package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

// OpenLocker returns the leader-election lock for the configured store backend,
// keyed to the Slack identity this node serves.
//
// The backend is not a separate choice: it follows `database.backend`, because
// nodes competing to serve one Slack app must agree on where the lock lives,
// and the only thing they are already guaranteed to agree on is the store their
// shared configuration came from. Letting the lock be configured independently
// would permit two nodes to hold two different "the" locks.
//
// Every backend supplies one, and the backend decides what kind. SQLite is
// local by construction, so it gets an OS advisory lock that guarantees one
// gateway per machine; Postgres and Firestore are reachable from anywhere, so
// they get a renewed lease that guarantees one gateway per cluster. There is no
// "no locker" option, because a node that serves without contending is the
// duplicate gateway the whole mechanism exists to prevent.
//
// ErrLockUnsupported therefore reports a genuinely unserviceable combination —
// today only a non-unix host on the SQLite backend — rather than a backend
// nobody has got round to.
//
// ttl sets the lease length for a backend that needs one; it is ignored by a
// backend whose liveness the OS guarantees. Zero selects DefaultLeaseTTL.
func OpenLocker(ctx context.Context, dbc config.DatabaseConfig, identity config.LockIdentity, ttl time.Duration) (config.Locker, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	switch backend := dbc.EffectiveBackend(); backend {
	case config.BackendSQLite:
		return openLocalLocker(config.LockDir(), identity)
	case config.BackendFirestore:
		return openFirestoreLocker(ctx, dbc.Firestore, identity, ttl)
	case config.BackendPostgres:
		return openPostgresLocker(ctx, strings.TrimSpace(dbc.Postgres.DSN), identity, ttl)
	default:
		return nil, fmt.Errorf("unknown database backend %q (want sqlite, postgres, or firestore)", dbc.Backend)
	}
}
