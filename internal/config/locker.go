package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// This file defines the leader-election seam. Exactly one Murtaugh gateway may
// hold a live Slack socket for a given Slack app: two connected gateways both
// receive every event and answer twice. The Locker is what enforces that, and
// its backend follows `database.backend` — the same store the configuration
// lives in, so competing nodes cannot disagree about where the lock is.
//
// The two backends deliberately guarantee different things:
//
//   - SQLite (local) — one gateway per machine. Liveness is the OS's job: the
//     lock is an advisory file lock the kernel drops when the process dies, so
//     there is no expiry to tune and no window in which a dead holder still
//     looks alive. It cannot coordinate separate machines and does not try.
//   - Firestore (distributed) — one gateway across a cluster. A remote store
//     cannot observe process death, so liveness degrades to a lease with an
//     expiry that the holder must keep renewing.
//
// Anything cheaper than a strongly consistent store (object-storage "locks" and
// the like) is a tradeoff for the admin to weigh, not one for Murtaugh to make
// on their behalf.

// LockIdentity names what is being made exclusive: one gateway per Slack app
// per workspace.
//
// It is derived from auth.test (team + app), NOT from the bot token. Hashing
// the token would tie lock identity to a credential that rotates: after a
// rotation the incumbent still holds the lock under the old token's hash while
// a new node acquires a different lock under the new hash, and both connect to
// the same Slack app — the exact double-reply failure the lock exists to
// prevent. Team and app are stable across rotation, and they are also the
// honest statement of the invariant. auth.test is a plain Web API call with no
// socket attached, so every node may safely make it before contending.
type LockIdentity struct {
	// TeamID is the Slack workspace ID (auth.test `team_id`).
	TeamID string
	// AppID is the Slack app ID (auth.test `bot_id`'s app, or the app-level
	// token's app). Two distinct apps in one workspace are independent gateways.
	AppID string
}

// Validate reports whether the identity is usable as a lock key.
func (i LockIdentity) Validate() error {
	if strings.TrimSpace(i.TeamID) == "" {
		return errors.New("lock identity: team ID is required")
	}
	if strings.TrimSpace(i.AppID) == "" {
		return errors.New("lock identity: app ID is required")
	}
	return nil
}

// Key renders the identity as a stable opaque token, safe to use as a filename
// component or a Firestore document ID. It is a hash rather than the raw IDs so
// that neither the lock file's name on a shared temp dir nor a document path in
// a shared project discloses which workspace this node serves.
func (i LockIdentity) Key() string {
	sum := sha256.Sum256([]byte("murtaugh-leader/v1|team=" + i.TeamID + "|app=" + i.AppID))
	return hex.EncodeToString(sum[:16])
}

// String renders the identity for logs and journal entries. Team and app IDs
// are not secrets, so they are shown plainly — the hash would be useless to a
// human reading a failover notice.
func (i LockIdentity) String() string {
	return i.TeamID + "/" + i.AppID
}

// Lease is a held claim on the leader lock.
//
// ExpiresAt is the field that distinguishes the two backends: it is the zero
// time for a lock whose liveness the OS guarantees (the local backend), and a
// server-assigned deadline for one that must be renewed (Firestore). Callers
// must branch on Locker.TTL rather than on this field so the intent is explicit.
type Lease struct {
	// Key is the LockIdentity.Key this lease was taken against.
	Key string
	// Owner identifies the holding node, for diagnostics and for the takeover
	// announcement. It is not used to decide ownership — the store is.
	Owner string
	// Epoch increments on every successful acquisition of the lock, so a
	// takeover is totally ordered and the journal can prove which node held it
	// when. It is a fencing token in the strict sense only where a downstream
	// can check it; Slack cannot, which is why the outbound gate exists.
	Epoch int64
	// ExpiresAt is when the lease lapses, as measured by the STORE's clock, not
	// this node's. Zero means the lease does not expire.
	ExpiresAt time.Time
}

// Held reports whether the lease is a real claim rather than a zero value.
func (l Lease) Held() bool { return strings.TrimSpace(l.Key) != "" }

// Expires reports whether this lease must be renewed to stay valid.
func (l Lease) Expires() bool { return !l.ExpiresAt.IsZero() }

// Locker is the backend-agnostic leader-election seam, implemented in
// internal/config/store over a local advisory file lock (SQLite) and Firestore.
//
// Implementations must be safe for concurrent use: the renewal loop and the
// shutdown path call into the same Locker from different goroutines.
type Locker interface {
	// Acquire attempts to take the lock. It reports ok=false — with a nil error
	// — when the lock is validly held by another node, which is an ordinary
	// outcome for a standby, not a failure. A non-nil error means the lock's
	// state could not be determined at all.
	Acquire(ctx context.Context) (lease Lease, ok bool, err error)

	// Renew extends a held lease and returns the extended one. It reports
	// ok=false when the lease has been lost — taken over, or expired past
	// recovery — which obliges the caller to demote immediately. A Locker whose
	// TTL is zero returns the lease unchanged.
	Renew(ctx context.Context, lease Lease) (renewed Lease, ok bool, err error)

	// Verify re-reads the lock and reports whether this lease is still the live
	// claim. It is the synchronous check the outbound gate makes before the
	// first Slack write following any gap in renewals, when elapsed-time
	// measurement on this node cannot be trusted (see Lease.Epoch).
	Verify(ctx context.Context, lease Lease) (ok bool, err error)

	// Release relinquishes a held lease so a standby can promote immediately
	// instead of waiting out the TTL. It is idempotent and safe to call with a
	// lease that has already been lost.
	Release(ctx context.Context, lease Lease) error

	// TTL is how long an acquired lease stays valid without renewal. Zero means
	// the lock does not expire because the backend detects holder death
	// directly — the caller then skips the whole renew/self-demote machinery.
	TTL() time.Duration

	// Backend reports the locker backend name, matching Store.Backend.
	Backend() string

	// Close releases any held lease and frees the backend handle.
	Close() error
}

// OwnerID builds the Owner string recorded with a lease: enough to identify the
// node in a takeover announcement without being load-bearing for correctness.
// The PID distinguishes two processes on one host, which is precisely the case
// the local backend exists to catch.
func OwnerID() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}

// LockDir is where the local backend keeps its lock files: the XDG state
// directory, deliberately NOT the config directory.
//
// The config directory is the wrong home for this. A lock living beside the
// store would only ever contend between gateways that already share a store,
// and the accident worth catching is the opposite one — a second daemon coming
// up under its own sandbox config, pointed at the same Slack app. A fixed
// location keyed by Slack identity catches that; a per-config one does not.
func LockDir() string { return journalStateDir() }

// ErrLockUnsupported is returned by OpenLocker for a backend that cannot
// provide the requested scope of exclusion — notably a distributed deployment
// asking the local backend to coordinate separate machines.
var ErrLockUnsupported = errors.New("leader election is not supported by this database backend")
