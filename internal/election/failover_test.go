package election

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/store"
)

// TestAllowIsClosedBeforeDemoteRuns is the ordering the gateway's stand-down
// depends on, and it spans two packages, so it is asserted here rather than
// assumed.
//
// StopServing disconnects the socket and drains agent turns, and it argues that
// none of that can leak a Slack message because the gate is already shut. That
// argument is only sound if leadership is cleared BEFORE the demote callback
// fires. If the order were reversed, every in-flight writer would have the
// whole drain window to post from a node that has already lost the election.
func TestAllowIsClosedBeforeDemoteRuns(t *testing.T) {
	locker, clock := newFakeLocker(), newFakeClock()

	var (
		allowedDuringDemote atomic.Bool
		demoteRan           atomic.Bool
	)
	var r *Runner
	r = newTestRunner(t, locker, clock, Callbacks{
		OnDemote: func(ctx context.Context, _ string) {
			demoteRan.Store(true)
			allowedDuringDemote.Store(r.Allow(ctx))
		},
	})

	ctx := context.Background()
	r.step(ctx)
	if !r.Leading() {
		t.Fatal("setup: not leading")
	}

	locker.set(func(l *fakeLocker) { l.renewOK = false })
	r.step(ctx)

	if !demoteRan.Load() {
		t.Fatal("the demote callback never ran")
	}
	if allowedDuringDemote.Load() {
		t.Fatal("Allow said yes while demotion was in progress; the drain window would leak Slack writes")
	}
}

// TestDemoteCallbackRunsBeforeTheLockIsReleased checks the other half of the
// handover ordering: the successor must not be able to promote while the
// outgoing leader is still tearing down.
func TestDemoteCallbackRunsBeforeTheLockIsReleased(t *testing.T) {
	locker, clock := newFakeLocker(), newFakeClock()

	var releasedBeforeDemote atomic.Bool
	r := newTestRunner(t, locker, clock, Callbacks{
		OnDemote: func(context.Context, string) {
			_, _, _, releases := locker.counts()
			releasedBeforeDemote.Store(releases > 0)
		},
	})

	ctx := context.Background()
	r.step(ctx)
	locker.set(func(l *fakeLocker) { l.renewOK = false })
	r.step(ctx)

	if releasedBeforeDemote.Load() {
		t.Error("the lock was released before demotion finished; a successor could promote mid-teardown")
	}
	if _, _, _, releases := locker.counts(); releases != 1 {
		t.Errorf("Release ran %d times, want 1", releases)
	}
}

// firestoreFailoverConfig points at the emulator, skipping when it is not up.
func firestoreFailoverConfig(t *testing.T) config.FirestoreConfig {
	t.Helper()
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("set FIRESTORE_EMULATOR_HOST to run the end-to-end failover test")
	}
	return config.FirestoreConfig{
		ProjectID:  "murtaugh-test",
		Collection: fmt.Sprintf("failover_%d", time.Now().UnixNano()),
	}
}

// node is one participant in the failover test: a runner plus a record of what
// it was told to do.
type node struct {
	name     string
	runner   *Runner
	locker   config.Locker
	mu       sync.Mutex
	promoted int
	demoted  int
}

func (n *node) counts() (int, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.promoted, n.demoted
}

// TestFailoverBetweenTwoNodes is the end-to-end proof of the whole feature,
// against a real Firestore: two nodes contend, exactly one leads, and when the
// leader goes away the other takes over — without both ever being allowed to
// act at once, which is the failure the design exists to prevent.
func TestFailoverBetweenTwoNodes(t *testing.T) {
	fsc := firestoreFailoverConfig(t)
	identity := config.LockIdentity{TeamID: "T0FAILOVER", AppID: "B0FAILOVER"}
	election := config.ElectionConfig{LeaseSeconds: 2, RenewSeconds: 1}

	newNode := func(name string) *node {
		locker, err := store.OpenLocker(context.Background(),
			config.DatabaseConfig{Backend: config.BackendFirestore, Firestore: fsc},
			identity, election.EffectiveLease())
		if err != nil {
			t.Fatalf("%s: open locker: %v", name, err)
		}
		t.Cleanup(func() { _ = locker.Close() })

		n := &node{name: name, locker: locker}
		r, err := New(Options{
			Locker:   locker,
			Election: election,
			Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			Callbacks: Callbacks{
				OnPromote: func(context.Context, config.Lease) error {
					n.mu.Lock()
					defer n.mu.Unlock()
					n.promoted++
					return nil
				},
				OnDemote: func(context.Context, string) {
					n.mu.Lock()
					defer n.mu.Unlock()
					n.demoted++
				},
			},
		})
		if err != nil {
			t.Fatalf("%s: new runner: %v", name, err)
		}
		n.runner = r
		return n
	}

	a, b := newNode("a"), newNode("b")

	ctxA, stopA := context.WithCancel(context.Background())
	ctxB, stopB := context.WithCancel(context.Background())
	// Both are deferred as well as cancelled explicitly below: a t.Fatal on any
	// intermediate assertion would otherwise leave a runner goroutine holding a
	// live lease against the emulator, which the next subtest would inherit.
	defer stopA()
	defer stopB()

	doneA := make(chan struct{})
	doneB := make(chan struct{})
	go func() { defer close(doneA); _ = a.runner.Run(ctxA) }()
	go func() { defer close(doneB); _ = b.runner.Run(ctxB) }()

	leader := awaitSingleLeader(t, a, b, 10*time.Second)
	follower := a
	if leader == a {
		follower = b
	}
	t.Logf("initial leader: %s", leader.name)

	// The leader goes away cleanly, which releases its lease.
	if leader == a {
		stopA()
		<-doneA
	} else {
		stopB()
		<-doneB
	}

	// The survivor must take over. It has to wait for its own retry tick, so
	// allow several lease periods.
	deadline := time.Now().Add(15 * time.Second)
	for !follower.runner.Leading() {
		if time.Now().After(deadline) {
			t.Fatalf("%s never took over after the leader left", follower.name)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if promoted, _ := follower.counts(); promoted != 1 {
		t.Errorf("%s promoted %d times, want 1", follower.name, promoted)
	}
	if !follower.runner.Allow(context.Background()) {
		t.Error("the new leader is not allowed to act")
	}
	if lease := follower.runner.Lease(); lease.Epoch < 2 {
		t.Errorf("takeover epoch = %d, want at least 2 (it followed a predecessor)", lease.Epoch)
	}
}

// awaitSingleLeader waits until exactly one of the two nodes leads, failing if
// both ever do — the invariant the whole feature protects.
func awaitSingleLeader(t *testing.T, a, b *node, within time.Duration) *node {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		leadingA, leadingB := a.runner.Leading(), b.runner.Leading()
		if leadingA && leadingB {
			t.Fatal("both nodes claimed leadership at once")
		}
		if leadingA {
			return a
		}
		if leadingB {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatal("neither node took the lock")
		}
		time.Sleep(25 * time.Millisecond)
	}
}
