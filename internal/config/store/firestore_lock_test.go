package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

// openTestFirestoreLocker opens a locker against the emulator, sharing a root
// collection (and therefore a lock document) with every locker opened from the
// same fsc — which is how these tests stand in for separate nodes.
func openTestFirestoreLocker(t *testing.T, fsc config.FirestoreConfig, ttl time.Duration) config.Locker {
	t.Helper()
	locker, err := openFirestoreLocker(context.Background(), fsc, testIdentity(), ttl)
	if err != nil {
		t.Fatalf("openFirestoreLocker: %v", err)
	}
	t.Cleanup(func() { _ = locker.Close() })
	return locker
}

// TestFirestoreLockerExcludesSecondNode is the distributed counterpart of the
// local backend's exclusion test: two nodes, one Slack app, only one leader.
func TestFirestoreLockerExcludesSecondNode(t *testing.T) {
	fsc := firestoreTestConfig(t)
	ctx := context.Background()

	leader := openTestFirestoreLocker(t, fsc, DefaultLeaseTTL)
	lease, ok, err := leader.Acquire(ctx)
	if err != nil || !ok {
		t.Fatalf("leader Acquire: ok=%v err=%v", ok, err)
	}
	if !lease.Expires() {
		t.Error("a Firestore lease must carry a deadline; the caller would skip renewal")
	}

	standby := openTestFirestoreLocker(t, fsc, DefaultLeaseTTL)
	if _, ok, err := standby.Acquire(ctx); err != nil || ok {
		t.Fatalf("standby Acquire: ok=%v err=%v; want refused with no error", ok, err)
	}
}

// TestFirestoreLockerTakeoverInvalidatesOldHolder is the central safety test.
//
// After a takeover the previous holder must learn it has lost the lock from the
// renewal it was going to make anyway, rather than carrying on as a second
// leader. This is the failure that produces two gateways answering every Slack
// message, so it is checked from both sides: Renew must report the loss, and
// Verify must agree.
func TestFirestoreLockerTakeoverInvalidatesOldHolder(t *testing.T) {
	fsc := firestoreTestConfig(t)
	ctx := context.Background()

	// A short lease so the takeover is reachable without a long sleep.
	old := openTestFirestoreLocker(t, fsc, time.Second)
	lease, ok, err := old.Acquire(ctx)
	if err != nil || !ok {
		t.Fatalf("original Acquire: ok=%v err=%v", ok, err)
	}

	// The holder goes silent past its lease; a standby promotes. The challenger
	// takes a long lease deliberately: only the ORIGINAL holder's TTL needs to be
	// short for expiry to be reachable. Giving the challenger a one-second lease
	// too would race the several round-trips below against its own clock, and a
	// loaded runner would fail on a lease that legitimately lapsed.
	time.Sleep(1500 * time.Millisecond)
	challenger := openTestFirestoreLocker(t, fsc, time.Minute)
	taken, ok, err := challenger.Acquire(ctx)
	if err != nil || !ok {
		t.Fatalf("challenger Acquire after expiry: ok=%v err=%v; want promoted", ok, err)
	}
	if taken.Epoch <= lease.Epoch {
		t.Errorf("epoch did not advance on takeover: old=%d new=%d", lease.Epoch, taken.Epoch)
	}

	// The old holder comes back believing it still leads. Both paths must
	// refuse it.
	if _, ok, err := old.Renew(ctx, lease); err != nil || ok {
		t.Errorf("old holder's Renew: ok=%v err=%v; want a clean loss", ok, err)
	}
	if ok, err := old.Verify(ctx, lease); err != nil || ok {
		t.Errorf("old holder's Verify: ok=%v err=%v; want not held", ok, err)
	}

	// And the new holder must be unaffected by the loser's attempts.
	if ok, err := challenger.Verify(ctx, taken); err != nil || !ok {
		t.Errorf("new holder's Verify: ok=%v err=%v; want still held", ok, err)
	}
}

// TestFirestoreLockerRenewHoldsOffChallengers checks the other half: a holder
// that keeps renewing must keep the lock, so a healthy leader is never displaced
// merely because time passed.
func TestFirestoreLockerRenewHoldsOffChallengers(t *testing.T) {
	fsc := firestoreTestConfig(t)
	ctx := context.Background()

	leader := openTestFirestoreLocker(t, fsc, time.Second)
	lease, ok, err := leader.Acquire(ctx)
	if err != nil || !ok {
		t.Fatalf("Acquire: ok=%v err=%v", ok, err)
	}
	challenger := openTestFirestoreLocker(t, fsc, time.Second)

	// Renew well inside the lease, repeatedly, spanning more than one full TTL.
	for i := 0; i < 6; i++ {
		time.Sleep(300 * time.Millisecond)
		renewed, ok, err := leader.Renew(ctx, lease)
		if err != nil || !ok {
			t.Fatalf("renew %d: ok=%v err=%v; a healthy leader lost its lease", i, ok, err)
		}
		lease = renewed
		if _, ok, err := challenger.Acquire(ctx); err != nil || ok {
			t.Fatalf("challenger took the lock from a renewing leader at %d: ok=%v err=%v", i, ok, err)
		}
	}
}

// TestFirestoreLockerReleaseIsImmediate covers clean shutdown: releasing must
// hand over at once rather than making the successor wait out the TTL, which is
// the difference between a restart being invisible and being a 30-second gap.
func TestFirestoreLockerReleaseIsImmediate(t *testing.T) {
	fsc := firestoreTestConfig(t)
	ctx := context.Background()

	leader := openTestFirestoreLocker(t, fsc, time.Minute)
	lease, ok, err := leader.Acquire(ctx)
	if err != nil || !ok {
		t.Fatalf("Acquire: ok=%v err=%v", ok, err)
	}
	standby := openTestFirestoreLocker(t, fsc, time.Minute)
	if _, ok, _ := standby.Acquire(ctx); ok {
		t.Fatal("standby acquired while the lease was live")
	}

	if err := leader.Release(ctx, lease); err != nil {
		t.Fatalf("Release: %v", err)
	}
	promoted, ok, err := standby.Acquire(ctx)
	if err != nil || !ok {
		t.Fatalf("standby Acquire after release: ok=%v err=%v; want immediate promotion", ok, err)
	}
	if promoted.Epoch <= lease.Epoch {
		t.Errorf("epoch did not advance across a clean handover: %d then %d", lease.Epoch, promoted.Epoch)
	}
}

// TestFirestoreLockerReleaseAfterTakeoverIsHarmless guards a subtle hazard: a
// demoted node running its shutdown path must not delete the lock its successor
// now holds. The precondition on Release is what prevents it.
func TestFirestoreLockerReleaseAfterTakeoverIsHarmless(t *testing.T) {
	fsc := firestoreTestConfig(t)
	ctx := context.Background()

	old := openTestFirestoreLocker(t, fsc, time.Second)
	lease, ok, err := old.Acquire(ctx)
	if err != nil || !ok {
		t.Fatalf("Acquire: ok=%v err=%v", ok, err)
	}
	time.Sleep(1500 * time.Millisecond)

	challenger := openTestFirestoreLocker(t, fsc, time.Minute)
	taken, ok, err := challenger.Acquire(ctx)
	if err != nil || !ok {
		t.Fatalf("challenger Acquire: ok=%v err=%v", ok, err)
	}

	// The demoted node finally reaches its shutdown path.
	if err := old.Release(ctx, lease); err != nil {
		t.Errorf("stale Release should be a no-op, got: %v", err)
	}
	if ok, err := challenger.Verify(ctx, taken); err != nil || !ok {
		t.Errorf("a stale Release destroyed the successor's lock: ok=%v err=%v", ok, err)
	}
}

// TestFirestoreLockerEpochNeverRestarts pins the ordering property across both
// ways leadership changes hands. An earlier version released by deleting the
// lock document, which reset the epoch to 1 on the next acquisition — leaving
// two unrelated leases indistinguishable in the journal and making the epoch
// useless as the record of who held the lock when.
func TestFirestoreLockerEpochNeverRestarts(t *testing.T) {
	fsc := firestoreTestConfig(t)
	ctx := context.Background()

	var last int64
	for round := 0; round < 3; round++ {
		locker := openTestFirestoreLocker(t, fsc, time.Second)
		lease, ok, err := locker.Acquire(ctx)
		if err != nil || !ok {
			t.Fatalf("round %d Acquire: ok=%v err=%v", round, ok, err)
		}
		if lease.Epoch != last+1 {
			t.Fatalf("round %d: epoch = %d, want %d (the count must never restart)", round, lease.Epoch, last+1)
		}
		last = lease.Epoch

		// Alternate the two ways a leader stops leading: a clean handover, and
		// simply going silent until the lease lapses.
		if round%2 == 0 {
			if err := locker.Release(ctx, lease); err != nil {
				t.Fatalf("round %d Release: %v", round, err)
			}
		} else {
			time.Sleep(1500 * time.Millisecond)
		}
	}
}

// TestFirestoreLockerConcurrentAcquireElectsOne is the race the whole design
// exists to survive: many nodes starting at once against a free lock. Exactly
// one may win, and the winners must not disagree about the epoch.
func TestFirestoreLockerConcurrentAcquireElectsOne(t *testing.T) {
	fsc := firestoreTestConfig(t)
	ctx := context.Background()

	const nodes = 8
	lockers := make([]config.Locker, nodes)
	for i := range lockers {
		lockers[i] = openTestFirestoreLocker(t, fsc, time.Minute)
	}

	var (
		mu      sync.Mutex
		winners []config.Lease
		errs    []error
		wg      sync.WaitGroup
		start   = make(chan struct{})
	)
	for _, locker := range lockers {
		wg.Add(1)
		go func(l config.Locker) {
			defer wg.Done()
			<-start
			// Retry a few times, exactly as the election runner does on its
			// next tick. A single attempt is not the contract: under real
			// contention Firestore aborts the losers' writes, and the useful
			// property is that repeated attempts converge on ONE holder — not
			// that the first round happens to produce one.
			for attempt := 0; attempt < 10; attempt++ {
				lease, ok, err := l.Acquire(ctx)
				if err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					return
				}
				if ok {
					mu.Lock()
					winners = append(winners, lease)
					mu.Unlock()
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}(locker)
	}
	close(start)
	wg.Wait()

	for _, err := range errs {
		t.Errorf("Acquire returned an error during a contended race: %v", err)
	}
	if len(winners) != 1 {
		t.Fatalf("%d nodes won the election, want exactly 1: %+v", len(winners), winners)
	}
}

// TestFirestoreLockerJudgesByRecordedLease checks a challenger honours the lease
// length the incumbent actually took the lock on, not its own configured TTL.
// Otherwise a node misconfigured with a short TTL would evict a healthy leader
// that is renewing perfectly well on the longer one.
func TestFirestoreLockerJudgesByRecordedLease(t *testing.T) {
	fsc := firestoreTestConfig(t)
	ctx := context.Background()

	// The incumbent takes a long lease.
	leader := openTestFirestoreLocker(t, fsc, time.Minute)
	if _, ok, err := leader.Acquire(ctx); err != nil || !ok {
		t.Fatalf("leader Acquire: ok=%v err=%v", ok, err)
	}

	// A challenger configured with a much shorter one must still back off.
	impatient := openTestFirestoreLocker(t, fsc, 500*time.Millisecond)
	time.Sleep(time.Second) // past the challenger's TTL, well inside the leader's
	if _, ok, err := impatient.Acquire(ctx); err != nil || ok {
		t.Fatalf("a short-TTL challenger evicted a healthy leader: ok=%v err=%v", ok, err)
	}
}

// TestFirestoreLockerVerifyRejectsForeignLease checks Verify keys on the epoch
// rather than the owner string. A restarted node can present the same host/pid
// owner as its predecessor, and must not thereby recognise a successor's lock
// as its own.
func TestFirestoreLockerVerifyRejectsForeignLease(t *testing.T) {
	fsc := firestoreTestConfig(t)
	ctx := context.Background()

	leader := openTestFirestoreLocker(t, fsc, time.Minute)
	lease, ok, err := leader.Acquire(ctx)
	if err != nil || !ok {
		t.Fatalf("Acquire: ok=%v err=%v", ok, err)
	}

	stale := lease
	stale.Epoch = lease.Epoch - 1
	if ok, err := leader.Verify(ctx, stale); err != nil || ok {
		t.Errorf("Verify accepted an older epoch: ok=%v err=%v", ok, err)
	}

	future := lease
	future.Epoch = lease.Epoch + 1
	if ok, err := leader.Verify(ctx, future); err != nil || ok {
		t.Errorf("Verify accepted an unissued epoch: ok=%v err=%v", ok, err)
	}

	if ok, err := leader.Verify(ctx, config.Lease{}); err != nil || ok {
		t.Errorf("Verify accepted a zero lease: ok=%v err=%v", ok, err)
	}
}

// TestFirestoreLockerTTLDefaults pins that a caller passing no TTL gets the
// documented default rather than a zero lease, which on this backend would mean
// "already expired" and produce continuous takeovers.
func TestFirestoreLockerTTLDefaults(t *testing.T) {
	fsc := firestoreTestConfig(t)
	locker := openTestFirestoreLocker(t, fsc, 0)
	if got := locker.TTL(); got != DefaultLeaseTTL {
		t.Errorf("TTL() = %v, want the default %v", got, DefaultLeaseTTL)
	}
	if got := locker.Backend(); got != config.BackendFirestore {
		t.Errorf("Backend() = %q, want %q", got, config.BackendFirestore)
	}
}

// TestOpenLockerSelectsFirestore checks the seam dispatches on database.backend,
// which is what keeps the lock in the same store as the config it came from.
func TestOpenLockerSelectsFirestore(t *testing.T) {
	fsc := firestoreTestConfig(t)
	locker, err := OpenLocker(context.Background(),
		config.DatabaseConfig{Backend: config.BackendFirestore, Firestore: fsc},
		testIdentity(), 0)
	if err != nil {
		t.Fatalf("OpenLocker(firestore): %v", err)
	}
	defer locker.Close()
	if got := locker.Backend(); got != config.BackendFirestore {
		t.Errorf("Backend() = %q, want %q", got, config.BackendFirestore)
	}
}
