package election

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

// fakeClock drives wall and monotonic time independently, which is the whole
// point: a real suspension advances wall time while monotonic time stands
// still, and no amount of arithmetic on a single clock can reproduce that.
type fakeClock struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func newFakeClock() *fakeClock {
	return &fakeClock{wall: time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Wall() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

func (c *fakeClock) Mono() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}

// advance moves both clocks forward together: ordinary elapsed time.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
	c.mono += d
}

// suspend simulates the machine sleeping: wall time passes, monotonic time
// barely moves, exactly as it does across a closed laptop lid.
func (c *fakeClock) suspend(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = c.wall.Add(d)
	c.mono += 2 * time.Millisecond
}

// fakeLocker is a scriptable config.Locker.
type fakeLocker struct {
	mu sync.Mutex

	ttl time.Duration

	acquireOK  bool
	acquireErr error
	renewOK    bool
	renewErr   error
	verifyOK   bool
	verifyErr  error
	releaseErr error

	epoch    int64
	acquires int
	renews   int
	verifies int
	releases int
}

func newFakeLocker() *fakeLocker {
	return &fakeLocker{ttl: 30 * time.Second, acquireOK: true, renewOK: true, verifyOK: true}
}

func (l *fakeLocker) Acquire(context.Context) (config.Lease, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.acquires++
	if l.acquireErr != nil {
		return config.Lease{}, false, l.acquireErr
	}
	if !l.acquireOK {
		return config.Lease{}, false, nil
	}
	l.epoch++
	return config.Lease{Key: "k", Owner: "node/1", Epoch: l.epoch}, true, nil
}

func (l *fakeLocker) Renew(_ context.Context, lease config.Lease) (config.Lease, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.renews++
	if l.renewErr != nil {
		return config.Lease{}, false, l.renewErr
	}
	if !l.renewOK {
		return config.Lease{}, false, nil
	}
	return lease, true, nil
}

func (l *fakeLocker) Verify(context.Context, config.Lease) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.verifies++
	if l.verifyErr != nil {
		return false, l.verifyErr
	}
	return l.verifyOK, nil
}

func (l *fakeLocker) Release(context.Context, config.Lease) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.releases++
	return l.releaseErr
}

func (l *fakeLocker) TTL() time.Duration { return l.ttl }
func (l *fakeLocker) Backend() string    { return "fake" }
func (l *fakeLocker) Close() error       { return nil }

func (l *fakeLocker) counts() (acquires, renews, verifies, releases int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.acquires, l.renews, l.verifies, l.releases
}

func (l *fakeLocker) set(f func(*fakeLocker)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f(l)
}

// transitions records promote/demote callbacks.
type transitions struct {
	mu       sync.Mutex
	promoted int
	demoted  int
	reasons  []string
	promErr  error
}

func (tr *transitions) callbacks() Callbacks {
	return Callbacks{
		OnPromote: func(context.Context, config.Lease) error {
			tr.mu.Lock()
			defer tr.mu.Unlock()
			tr.promoted++
			return tr.promErr
		},
		OnDemote: func(_ context.Context, reason string) {
			tr.mu.Lock()
			defer tr.mu.Unlock()
			tr.demoted++
			tr.reasons = append(tr.reasons, reason)
		},
	}
}

func (tr *transitions) counts() (promoted, demoted int) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.promoted, tr.demoted
}

// newTestRunner builds a runner with a 30s lease renewed every 10s, so
// demoteAfter is 20s.
func newTestRunner(t *testing.T, locker config.Locker, clock Clock, cb Callbacks) *Runner {
	t.Helper()
	r, err := New(Options{
		Locker:    locker,
		Callbacks: cb,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:     clock,
		Fallback:  config.FallbackConfig{Enabled: true, LeaseSeconds: 30, RenewSeconds: 10},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// TestRunnerPromotesWhenLockIsFree covers the ordinary start-up path.
func TestRunnerPromotesWhenLockIsFree(t *testing.T) {
	locker, clock, tr := newFakeLocker(), newFakeClock(), &transitions{}
	r := newTestRunner(t, locker, clock, tr.callbacks())

	r.step(context.Background())

	if !r.Leading() {
		t.Fatal("runner did not take a free lock")
	}
	if promoted, _ := tr.counts(); promoted != 1 {
		t.Errorf("OnPromote fired %d times, want exactly 1", promoted)
	}
	if !r.Allow(context.Background()) {
		t.Error("a freshly promoted leader was not allowed to act")
	}
}

// TestRunnerStaysStandbyWhenHeld covers the other node's view: losing is normal
// and must produce no callbacks and no noise.
func TestRunnerStaysStandbyWhenHeld(t *testing.T) {
	locker, clock, tr := newFakeLocker(), newFakeClock(), &transitions{}
	locker.set(func(l *fakeLocker) { l.acquireOK = false })
	r := newTestRunner(t, locker, clock, tr.callbacks())

	r.step(context.Background())

	if r.Leading() {
		t.Fatal("runner claimed leadership while the lock was held")
	}
	if promoted, demoted := tr.counts(); promoted != 0 || demoted != 0 {
		t.Errorf("standby fired callbacks: promoted=%d demoted=%d", promoted, demoted)
	}
	if r.Allow(context.Background()) {
		t.Error("a standby was allowed to act as leader")
	}
}

// TestRunnerAcquireErrorDoesNotPromote pins the fail-closed reading of an
// unreachable store: not knowing whether the lock is free is not the same as it
// being free.
func TestRunnerAcquireErrorDoesNotPromote(t *testing.T) {
	locker, clock, tr := newFakeLocker(), newFakeClock(), &transitions{}
	locker.set(func(l *fakeLocker) { l.acquireErr = errors.New("network down") })
	r := newTestRunner(t, locker, clock, tr.callbacks())

	r.step(context.Background())

	if r.Leading() {
		t.Fatal("runner promoted itself despite being unable to read the lock")
	}
}

// TestRunnerStandsDownWhenLeaseIsTaken covers the unambiguous loss: the store
// says another node holds the lock, so this one must stop immediately rather
// than waiting for a deadline.
func TestRunnerStandsDownWhenLeaseIsTaken(t *testing.T) {
	locker, clock, tr := newFakeLocker(), newFakeClock(), &transitions{}
	r := newTestRunner(t, locker, clock, tr.callbacks())
	ctx := context.Background()

	r.step(ctx)
	if !r.Leading() {
		t.Fatal("setup: not leading")
	}

	locker.set(func(l *fakeLocker) { l.renewOK = false })
	r.step(ctx)

	if r.Leading() {
		t.Fatal("runner kept leading after the lock was taken")
	}
	if _, demoted := tr.counts(); demoted != 1 {
		t.Errorf("OnDemote fired %d times, want exactly 1", demoted)
	}
	if _, _, _, releases := locker.counts(); releases != 1 {
		t.Errorf("Release called %d times on stand-down, want 1", releases)
	}
}

// TestRunnerToleratesTransientRenewalFailure is the flapping guard: a single
// failed renewal must not cost leadership, because a takeover tears down
// in-flight work and is worse than the blip it responds to.
func TestRunnerToleratesTransientRenewalFailure(t *testing.T) {
	locker, clock, tr := newFakeLocker(), newFakeClock(), &transitions{}
	r := newTestRunner(t, locker, clock, tr.callbacks())
	ctx := context.Background()

	r.step(ctx)
	locker.set(func(l *fakeLocker) { l.renewErr = errors.New("timeout") })

	// One failed renewal, well inside the 20s unconfirmed deadline.
	clock.advance(10 * time.Second)
	r.step(ctx)

	if !r.Leading() {
		t.Fatal("a single failed renewal cost the node its leadership")
	}
	if _, demoted := tr.counts(); demoted != 0 {
		t.Errorf("OnDemote fired %d times during a transient failure", demoted)
	}

	// Recovery restores the confirmed timestamp.
	locker.set(func(l *fakeLocker) { l.renewErr = nil })
	clock.advance(10 * time.Second)
	r.step(ctx)
	if !r.Leading() {
		t.Error("leadership lost after renewals recovered")
	}
}

// TestRunnerStandsDownBeforeTheLeaseExpires is the central timing property.
//
// The node must stop acting one renewal interval BEFORE its claim lapses, not
// when it lapses. Standing down at expiry would leave this node acting for as
// long as it took to notice, overlapping with the challenger that is by then
// entitled to promote — and during that overlap both would answer Slack.
func TestRunnerStandsDownBeforeTheLeaseExpires(t *testing.T) {
	locker, clock, tr := newFakeLocker(), newFakeClock(), &transitions{}
	r := newTestRunner(t, locker, clock, tr.callbacks())
	ctx := context.Background()

	r.step(ctx)
	locker.set(func(l *fakeLocker) { l.renewErr = errors.New("partitioned") })

	// 19s of failed renewals: inside the 20s deadline, so still leading.
	clock.advance(19 * time.Second)
	r.step(ctx)
	if !r.Leading() {
		t.Fatal("stood down early, before the unconfirmed deadline")
	}

	// Crossing 20s — still 10s short of the 30s lease — must stand it down.
	clock.advance(2 * time.Second)
	r.step(ctx)
	if r.Leading() {
		t.Fatal("still leading past the unconfirmed deadline; it would overlap with its successor")
	}
	if _, demoted := tr.counts(); demoted != 1 {
		t.Errorf("OnDemote fired %d times, want 1", demoted)
	}
}

// TestAllowDetectsSuspension is the test this design exists for.
//
// A leader's process is suspended past its lease. Another node promotes. The
// laptop wakes. Every purely local check the node can make says it is still
// leader, because the monotonic clock it would consult was suspended too:
// time.Since across the gap reads as milliseconds. Only asking the store gets
// the right answer, and Allow must ask.
func TestAllowDetectsSuspension(t *testing.T) {
	locker, clock, tr := newFakeLocker(), newFakeClock(), &transitions{}
	r := newTestRunner(t, locker, clock, tr.callbacks())
	ctx := context.Background()

	r.step(ctx)
	if !r.Leading() {
		t.Fatal("setup: not leading")
	}
	_, _, verifiesBefore, _ := locker.counts()

	// The lid closes for four hours. Monotonic time barely moves.
	clock.suspend(4 * time.Hour)

	// Confirm the trap is real: by the monotonic measure alone, no time passed.
	if _, mono := r.confirmed.since(now(clock)); mono > time.Second {
		t.Fatalf("fake suspend advanced monotonic time by %v; it must stay near zero to model a real one", mono)
	}

	// Meanwhile another node took over.
	locker.set(func(l *fakeLocker) { l.verifyOK = false })

	if r.Allow(ctx) {
		t.Fatal("a resumed node was allowed to act as leader after losing the lock — this is the duplicate-gateway bug")
	}
	if _, _, verifiesAfter, _ := locker.counts(); verifiesAfter <= verifiesBefore {
		t.Error("Allow answered from cache after a suspension instead of asking the store")
	}
	if r.Leading() {
		t.Error("runner still believes it leads after a failed verification")
	}
	if _, demoted := tr.counts(); demoted != 1 {
		t.Errorf("OnDemote fired %d times after the suspension, want 1", demoted)
	}
}

// TestAllowAfterSuspensionKeepsLeadershipWhenStillHeld is the counterpart: a
// suspension that did NOT cost the lock must not cost leadership either. Waking
// up is not by itself a demotion.
func TestAllowAfterSuspensionKeepsLeadershipWhenStillHeld(t *testing.T) {
	locker, clock, tr := newFakeLocker(), newFakeClock(), &transitions{}
	r := newTestRunner(t, locker, clock, tr.callbacks())
	ctx := context.Background()

	r.step(ctx)
	clock.suspend(time.Hour)

	if !r.Allow(ctx) {
		t.Fatal("a resumed leader that still holds the lock was refused")
	}
	if _, demoted := tr.counts(); demoted != 0 {
		t.Errorf("OnDemote fired %d times for a leader that never lost the lock", demoted)
	}
}

// TestAllowUsesCacheWhenFresh keeps the gate off the network on the hot path: a
// lease confirmed a moment ago is still good, and every Slack write should not
// cost a store round trip.
func TestAllowUsesCacheWhenFresh(t *testing.T) {
	locker, clock, tr := newFakeLocker(), newFakeClock(), &transitions{}
	r := newTestRunner(t, locker, clock, tr.callbacks())
	ctx := context.Background()

	r.step(ctx)
	_, _, before, _ := locker.counts()

	clock.advance(time.Second)
	for i := 0; i < 20; i++ {
		if !r.Allow(ctx) {
			t.Fatalf("call %d: a fresh leader was refused", i)
		}
	}

	if _, _, after, _ := locker.counts(); after != before {
		t.Errorf("Allow made %d store calls for a freshly confirmed lease; want 0", after-before)
	}
}

// TestAllowVerifiesOnceRenewalIsOverdue covers the non-suspend staleness case: a
// missed renewal means the claim may already be gone, so the cached yes expires
// even though the clocks agree.
func TestAllowVerifiesOnceRenewalIsOverdue(t *testing.T) {
	locker, clock, tr := newFakeLocker(), newFakeClock(), &transitions{}
	r := newTestRunner(t, locker, clock, tr.callbacks())
	ctx := context.Background()

	r.step(ctx)
	_, _, before, _ := locker.counts()

	// Past one renewal interval, both clocks agreeing.
	clock.advance(11 * time.Second)
	if !r.Allow(ctx) {
		t.Fatal("leader refused while it still held the lock")
	}
	if _, _, after, _ := locker.counts(); after != before+1 {
		t.Errorf("Allow made %d verifications for an overdue lease, want 1", after-before)
	}
}

// TestAllowFailsClosedOnVerifyError pins the direction of the error bias. Being
// unable to prove leadership is not permission to act as leader: a false
// negative costs one dropped reply, a false positive costs a second gateway
// answering everything.
func TestAllowFailsClosedOnVerifyError(t *testing.T) {
	locker, clock, tr := newFakeLocker(), newFakeClock(), &transitions{}
	r := newTestRunner(t, locker, clock, tr.callbacks())
	ctx := context.Background()

	r.step(ctx)
	locker.set(func(l *fakeLocker) { l.verifyErr = errors.New("unreachable") })
	clock.suspend(time.Hour)

	if r.Allow(ctx) {
		t.Fatal("Allow permitted a leader action it could not verify")
	}
	// An unreachable store is not proof of loss, so leadership is retained and
	// the renewal loop's deadline decides — demoting here would hand leadership
	// away on a blip.
	if !r.Leading() {
		t.Error("an unverifiable check demoted the leader outright; that turns a blip into a failover")
	}
}

// TestNonExpiringLockerSkipsTheDeadline covers the local backend's shape: a lock
// the kernel releases on process death has no lease to renew, so no amount of
// elapsed time may demote the holder on a timer.
func TestNonExpiringLockerSkipsTheDeadline(t *testing.T) {
	locker, clock, tr := newFakeLocker(), newFakeClock(), &transitions{}
	locker.set(func(l *fakeLocker) { l.ttl = 0 })
	r := newTestRunner(t, locker, clock, tr.callbacks())
	ctx := context.Background()

	r.step(ctx)
	if !r.Leading() {
		t.Fatal("setup: not leading")
	}

	locker.set(func(l *fakeLocker) { l.renewErr = errors.New("should not matter") })
	clock.advance(time.Hour)
	r.step(ctx)

	if !r.Leading() {
		t.Error("a non-expiring lock was demoted on a timer; only process death releases it")
	}
	if _, demoted := tr.counts(); demoted != 0 {
		t.Errorf("OnDemote fired %d times for a non-expiring lock", demoted)
	}
}

// TestNonExpiringLockerStillVerifiesAfterSuspension checks the local backend is
// not exempt from the resume check. Its lock cannot lapse on a timer, but it can
// still be lost — the lock file replaced beneath it — and a suspension is
// exactly when this node's belief is least trustworthy.
func TestNonExpiringLockerStillVerifiesAfterSuspension(t *testing.T) {
	locker, clock, tr := newFakeLocker(), newFakeClock(), &transitions{}
	locker.set(func(l *fakeLocker) { l.ttl = 0 })
	r := newTestRunner(t, locker, clock, tr.callbacks())
	ctx := context.Background()

	r.step(ctx)
	_, _, before, _ := locker.counts()

	clock.suspend(2 * time.Hour)
	locker.set(func(l *fakeLocker) { l.verifyOK = false })

	if r.Allow(ctx) {
		t.Fatal("a resumed node kept acting on a lock it no longer holds")
	}
	if _, _, after, _ := locker.counts(); after <= before {
		t.Error("Allow skipped verification after a suspension on a non-expiring lock")
	}
}

// TestPromotionFailureReleasesTheLock guards against a node that cannot serve
// sitting on the lock other nodes are waiting for, which converts one node's
// startup failure into a total outage.
func TestPromotionFailureReleasesTheLock(t *testing.T) {
	locker, clock, tr := newFakeLocker(), newFakeClock(), &transitions{}
	tr.promErr = errors.New("cannot connect to Slack")
	r := newTestRunner(t, locker, clock, tr.callbacks())

	r.step(context.Background())

	if r.Leading() {
		t.Fatal("runner kept leadership after its promotion handler failed")
	}
	if _, _, _, releases := locker.counts(); releases != 1 {
		t.Errorf("Release called %d times after a failed promotion, want 1", releases)
	}
	if _, demoted := tr.counts(); demoted != 1 {
		t.Errorf("OnDemote fired %d times after a failed promotion, want 1", demoted)
	}
}

// TestStandDownIsIdempotent checks the callbacks fire once per promotion, so a
// demotion racing with shutdown does not tear the same state down twice.
func TestStandDownIsIdempotent(t *testing.T) {
	locker, clock, tr := newFakeLocker(), newFakeClock(), &transitions{}
	r := newTestRunner(t, locker, clock, tr.callbacks())
	ctx := context.Background()

	r.step(ctx)
	r.standDown(ctx, "first")
	r.standDown(ctx, "second")
	r.standDown(ctx, "third")

	if _, demoted := tr.counts(); demoted != 1 {
		t.Errorf("OnDemote fired %d times, want exactly 1", demoted)
	}
	if _, _, _, releases := locker.counts(); releases != 1 {
		t.Errorf("Release called %d times, want exactly 1", releases)
	}
}

// TestRunReleasesOnShutdown covers clean exit: a leader that is shutting down
// must free the lock so a standby promotes at once instead of waiting out the
// lease.
func TestRunReleasesOnShutdown(t *testing.T) {
	locker, clock, tr := newFakeLocker(), newFakeClock(), &transitions{}
	r := newTestRunner(t, locker, clock, tr.callbacks())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	// Wait for the first acquisition rather than sleeping a fixed interval.
	deadline := time.After(2 * time.Second)
	for !r.Leading() {
		select {
		case <-deadline:
			t.Fatal("runner never took the lock")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v; losing or ending an election is not an error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	if _, _, _, releases := locker.counts(); releases != 1 {
		t.Errorf("Release called %d times on shutdown, want 1", releases)
	}
	if _, demoted := tr.counts(); demoted != 1 {
		t.Errorf("OnDemote fired %d times on shutdown, want 1", demoted)
	}
}

// TestNewRequiresALocker guards the composition root against wiring a runner
// with nothing to contend on, which would silently make every node a leader.
func TestNewRequiresALocker(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New accepted a nil locker")
	}
}
