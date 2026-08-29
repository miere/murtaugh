package store

import (
	"context"
	"fmt"

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
// Not every store backend can arbitrate every scope, and the difference is
// reported rather than papered over — a backend that cannot coordinate separate
// machines says so with ErrLockUnsupported instead of silently degrading to a
// lock that only looks like it works.
func OpenLocker(_ context.Context, dbc config.DatabaseConfig, identity config.LockIdentity) (config.Locker, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	switch backend := dbc.EffectiveBackend(); backend {
	case config.BackendSQLite:
		return openLocalLocker(config.LockDir(), identity)
	case config.BackendPostgres:
		// Postgres could host a lease — but it is not wired yet, and guessing
		// would be worse than saying so. Until then a Postgres-backed
		// deployment gets no election rather than a broken one.
		return nil, fmt.Errorf("%w: %q", config.ErrLockUnsupported, backend)
	default:
		return nil, fmt.Errorf("unknown database backend %q (want sqlite or postgres)", dbc.Backend)
	}
}
