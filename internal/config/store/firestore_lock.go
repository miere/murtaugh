package store

import (
	"context"
	"fmt"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/miere/murtaugh/internal/config"
)

// firestoreLocker is the distributed leader lock: one Murtaugh gateway holding
// a live Slack socket across a cluster of nodes.
//
// Unlike the local backend it cannot observe a holder's death, so liveness
// degrades to a lease the holder must keep renewing. Everything below follows
// from that one concession.
//
// # Clocks
//
// No decision here uses this node's clock, and that is deliberate rather than
// fastidious. Lease expiry is a comparison between "when was the lock taken"
// and "what time is it now", and if either side comes from a competing node's
// own clock then skew between two nodes silently becomes disagreement about who
// leads. Both sides are therefore taken from Firestore:
//
//   - acquired_at is written with firestore.ServerTimestamp, so the server
//     stamps it.
//   - "now" is DocumentSnapshot.ReadTime, the server's clock at the moment it
//     served the read.
//
// A challenger's verdict is thus computed entirely from two server-sourced
// timestamps, and two nodes reading the same document reach the same verdict no
// matter how far apart their local clocks have drifted.
//
// # Compare-and-swap
//
// There is no transaction here because none is needed: Firestore's write
// preconditions already give the compare-and-swap this requires.
//
//   - Taking a lock nobody holds is Create, which fails if the document exists.
//   - Taking an expired lock is Update guarded by LastUpdateTime from the read
//     that observed the expiry, which fails if anyone touched the document in
//     between.
//
// Two nodes racing on an expired lease both attempt the second form with the
// same precondition, and exactly one wins. The precondition doubles as the
// fencing check on renewal: if a takeover has happened, the holder's next
// renewal fails against a document whose update time has moved, so the loser
// learns it lost from the write it was going to make anyway.
type firestoreLocker struct {
	client   *firestore.Client
	doc      *firestore.DocumentRef
	identity config.LockIdentity
	ttl      time.Duration
	// ownsClient records whether Close should shut the Firestore client down.
	// A locker opened alongside its own client owns it; one sharing the config
	// store's client must not close it out from under the store.
	ownsClient bool

	mu sync.Mutex
	// lastUpdate is the server-assigned update time of our most recent write,
	// used as the precondition for the next one. It is the whole of this
	// locker's mutable state: a node holds at most one lease at a time.
	lastUpdate time.Time
}

// Firestore lock document field names.
const (
	fsLockOwner      = "owner"
	fsLockEpoch      = "epoch"
	fsLockAcquiredAt = "acquired_at"
	fsLockLeaseSecs  = "lease_seconds"
	fsLockTeamID     = "team_id"
	fsLockAppID      = "app_id"
	fsLockReleased   = "released"
)

// DefaultLeaseTTL is how long a Firestore lease stays valid without renewal.
//
// It is a compromise between the two failure modes the draft named. Too short
// and an ordinary network hiccup hands leadership to another node for no
// reason, which is worse than the outage it is trying to avoid because a
// takeover tears down in-flight work. Too long and a genuinely dead node's
// silence stretches out visibly. Thirty seconds sits well outside a transient
// stall and well inside "did the bot die?".
const DefaultLeaseTTL = 30 * time.Second

// openFirestoreLocker prepares the distributed leader lock for identity. It
// opens its own Firestore client so the locker can be constructed before — or
// without — a config store, and does not contend for the lock until Acquire.
func openFirestoreLocker(ctx context.Context, fsc config.FirestoreConfig, identity config.LockIdentity, ttl time.Duration) (config.Locker, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	client, err := newFirestoreClient(ctx, fsc)
	if err != nil {
		return nil, err
	}
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	return &firestoreLocker{
		client:     client,
		doc:        client.Collection(fsc.EffectiveCollection() + "_locks").Doc(identity.Key()),
		identity:   identity,
		ttl:        ttl,
		ownsClient: true,
	}, nil
}

func (l *firestoreLocker) Backend() string { return config.BackendFirestore }

func (l *firestoreLocker) TTL() time.Duration { return l.ttl }

func (l *firestoreLocker) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.ownsClient {
		return nil
	}
	return l.client.Close()
}

// lockState is one read of the lock document, with the expiry verdict already
// resolved against the server's clock.
type lockState struct {
	exists     bool
	owner      string
	epoch      int64
	expired    bool
	updateTime time.Time
}

// read fetches the lock document and decides, in server time only, whether the
// recorded lease has lapsed.
func (l *firestoreLocker) read(ctx context.Context) (lockState, error) {
	snap, err := l.doc.Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return lockState{}, nil
		}
		return lockState{}, fmt.Errorf("read leader lock: %w", err)
	}

	state := lockState{exists: true, updateTime: snap.UpdateTime}
	if v, err := snap.DataAt(fsLockOwner); err == nil {
		state.owner, _ = v.(string)
	}
	if v, err := snap.DataAt(fsLockEpoch); err == nil {
		switch n := v.(type) {
		case int64:
			state.epoch = n
		case float64:
			state.epoch = int64(n)
		}
	}

	// A released lock is free at once, without waiting out its lease. The
	// document survives the release rather than being deleted, so the epoch
	// keeps counting across a clean handover; deleting it would restart the
	// count at 1 and destroy the ordering the epoch exists to provide.
	if v, err := snap.DataAt(fsLockReleased); err == nil {
		if released, _ := v.(bool); released {
			state.expired = true
			return state, nil
		}
	}

	acquired, err := snap.DataAt(fsLockAcquiredAt)
	if err != nil {
		// A document with no acquisition time cannot be shown to be live, and
		// leaving it un-takeable would wedge the cluster on a malformed row.
		state.expired = true
		return state, nil
	}
	acquiredAt, ok := acquired.(time.Time)
	if !ok {
		state.expired = true
		return state, nil
	}

	// The lease length is read from the document rather than assumed, so a node
	// configured with a different TTL still judges the incumbent's lease by the
	// terms the incumbent actually took it on.
	lease := l.ttl
	if v, err := snap.DataAt(fsLockLeaseSecs); err == nil {
		switch n := v.(type) {
		case int64:
			if n > 0 {
				lease = time.Duration(n) * time.Second
			}
		case float64:
			if n > 0 {
				lease = time.Duration(n) * time.Second
			}
		}
	}

	// Both sides of this comparison come from the server: acquiredAt was
	// stamped by it, and ReadTime is its clock at the moment it served this
	// read. This node's own clock is not consulted.
	state.expired = !snap.ReadTime.Before(acquiredAt.Add(lease))
	return state, nil
}

// lockDoc renders the document written on acquisition and renewal.
func (l *firestoreLocker) lockDoc(owner string, epoch int64) map[string]any {
	return map[string]any{
		fsLockOwner:      owner,
		fsLockEpoch:      epoch,
		fsLockAcquiredAt: firestore.ServerTimestamp,
		fsLockLeaseSecs:  int64(l.ttl / time.Second),
		fsLockTeamID:     l.identity.TeamID,
		fsLockAppID:      l.identity.AppID,
		fsLockReleased:   false,
	}
}

// updatesFor renders lockDoc as a field-update list, which is the form that
// accepts a precondition.
func (l *firestoreLocker) updatesFor(owner string, epoch int64) []firestore.Update {
	doc := l.lockDoc(owner, epoch)
	updates := make([]firestore.Update, 0, len(doc))
	for path, value := range doc {
		updates = append(updates, firestore.Update{Path: path, Value: value})
	}
	return updates
}

// Acquire takes the lock when it is free or its lease has lapsed. A lock validly
// held by another node yields ok=false and a nil error — the ordinary standby
// outcome. A losing CAS race yields the same, because losing a race is not an
// error either; the caller simply retries on its next tick.
func (l *firestoreLocker) Acquire(ctx context.Context) (config.Lease, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	state, err := l.read(ctx)
	if err != nil {
		return config.Lease{}, false, err
	}
	if state.exists && !state.expired {
		return config.Lease{}, false, nil
	}

	owner := config.OwnerID()
	epoch := state.epoch + 1

	var result *firestore.WriteResult
	if !state.exists {
		// Create fails if another node created the document first, which makes
		// it the compare-and-swap for an uncontested lock.
		result, err = l.doc.Create(ctx, l.lockDoc(owner, epoch))
	} else {
		// The lease has lapsed. Guard the takeover with the update time we just
		// observed, so a node that renewed or took over between our read and
		// our write wins and we back off.
		result, err = l.doc.Update(ctx, l.updatesFor(owner, epoch), firestore.LastUpdateTime(state.updateTime))
	}
	if err != nil {
		// Losing a race and being elbowed out of one are the same outcome to a
		// caller that will try again on its next tick.
		if lostRace(err) || contended(err) {
			return config.Lease{}, false, nil
		}
		return config.Lease{}, false, fmt.Errorf("acquire leader lock: %w", err)
	}

	l.lastUpdate = result.UpdateTime
	return l.leaseFrom(owner, epoch, result.UpdateTime), true, nil
}

// Renew extends a held lease.
//
// The LastUpdateTime precondition is what makes this safe: it fails unless the
// document is exactly as this node last left it, so a takeover during a network
// stall surfaces here as a lost lease rather than as two nodes each believing
// they lead. ok=false obliges the caller to demote at once.
func (l *firestoreLocker) Renew(ctx context.Context, lease config.Lease) (config.Lease, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !lease.Held() || lease.Key != l.identity.Key() || l.lastUpdate.IsZero() {
		return config.Lease{}, false, nil
	}

	result, err := l.doc.Update(ctx,
		l.updatesFor(lease.Owner, lease.Epoch),
		firestore.LastUpdateTime(l.lastUpdate))
	if err != nil {
		if lostRace(err) {
			return config.Lease{}, false, nil
		}
		// A transport failure is "cannot tell", not "lost". The caller keeps the
		// lease and retries, and its own self-demotion deadline is what stops it
		// holding on indefinitely.
		return config.Lease{}, false, fmt.Errorf("renew leader lock: %w", err)
	}

	l.lastUpdate = result.UpdateTime
	return l.leaseFrom(lease.Owner, lease.Epoch, result.UpdateTime), true, nil
}

// Verify re-reads the lock and reports whether this lease is still the live
// claim.
//
// This is the check that closes the zombie-leader hole, and it is synchronous
// against the store on purpose. A node whose process was suspended — a closed
// laptop lid, most plainly — cannot detect the gap by measuring elapsed time,
// because Go's monotonic clock does not advance while the machine is asleep:
// time.Since across a four-hour suspend reads as milliseconds. Any guard built
// on local elapsed time therefore passes at exactly the moment it is needed.
// Asking the server is the only answer that does not depend on this node having
// noticed the time pass.
func (l *firestoreLocker) Verify(ctx context.Context, lease config.Lease) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !lease.Held() || lease.Key != l.identity.Key() {
		return false, nil
	}
	state, err := l.read(ctx)
	if err != nil {
		return false, err
	}
	// Ownership is the epoch, not the owner string: a node restarting under the
	// same host and PID would otherwise recognise a successor's lock as its own.
	return state.exists && !state.expired && state.epoch == lease.Epoch && state.owner == lease.Owner, nil
}

// Release marks the lock free so a standby promotes at once rather than waiting
// out the TTL — the difference between a restart being invisible and being a
// TTL-long silence.
//
// It tombstones the document rather than deleting it, so the epoch survives the
// handover. Deleting would restart the count at 1 on the next acquisition, which
// destroys the total ordering the epoch exists to provide and makes two
// unrelated leases indistinguishable in the journal.
//
// The precondition keeps a demoted node from freeing a successor's lock: if we
// no longer hold it, the write simply does not apply.
func (l *firestoreLocker) Release(ctx context.Context, lease config.Lease) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !lease.Held() || lease.Key != l.identity.Key() || l.lastUpdate.IsZero() {
		return nil
	}
	last := l.lastUpdate
	l.lastUpdate = time.Time{}

	if _, err := l.doc.Update(ctx,
		[]firestore.Update{{Path: fsLockReleased, Value: true}},
		firestore.LastUpdateTime(last)); err != nil {
		if lostRace(err) {
			// Already taken over: there is nothing of ours left to release.
			return nil
		}
		return fmt.Errorf("release leader lock: %w", err)
	}
	return nil
}

// leaseFrom builds the lease handed back to the caller.
//
// ExpiresAt is expressed in the server's timeline, since that is the only
// timeline the deadline is meaningful in. It is advisory: the caller uses it to
// schedule renewals, NOT to decide whether it still leads. That decision goes
// through Verify, for the suspend reason documented there.
func (l *firestoreLocker) leaseFrom(owner string, epoch int64, serverTime time.Time) config.Lease {
	return config.Lease{
		Key:       l.identity.Key(),
		Owner:     owner,
		Epoch:     epoch,
		ExpiresAt: serverTime.Add(l.ttl),
	}
}

// lostRace reports whether err means "someone else got there first" rather than
// "the operation failed". Both Firestore precondition outcomes land here: an
// AlreadyExists from a losing Create, and a FailedPrecondition from an Update or
// Delete whose LastUpdateTime no longer matches.
func lostRace(err error) bool {
	switch status.Code(err) {
	case codes.AlreadyExists, codes.FailedPrecondition:
		return true
	default:
		return false
	}
}

// contended reports whether err means the write did not land because another
// writer held the document at that instant.
//
// Firestore surfaces this as ABORTED ("Transaction lock timeout"), and it is a
// RETRYABLE outcome rather than a failure: the document is unchanged and the
// caller may simply try again. Treating it as an error, as an earlier version
// did, turned ordinary contention into "the lock state cannot be determined" —
// which under a genuine race between several nodes starting together meant
// nobody promoted and every one of them logged a fault.
//
// It is deliberately consulted only where the caller is ATTEMPTING to take
// something (Acquire, and the job-run Claim). For a renewal, contention is
// ambiguous — it may be a takeover in progress — and the conservative reading
// already exists: keep the lease and let the stand-down deadline decide.
func contended(err error) bool { return status.Code(err) == codes.Aborted }
