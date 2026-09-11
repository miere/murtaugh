package credwarden

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// collector records every health crossing the warden reports.
type collector struct {
	mu   sync.Mutex
	seen []Health
}

func (c *collector) observe(h Health) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, h)
}

func (c *collector) snapshot() []Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Health(nil), c.seen...)
}

// observedWarden builds a warden whose reads fail on demand, with the observer
// attached.
func observedWarden(t *testing.T, failing *bool, expiry time.Time) (*Warden, *collector) {
	t.Helper()
	c := &collector{}
	w := New(Options{
		Identities:    []Identity{testID},
		DegradedAfter: 2,
		Observer:      c.observe,
		readExpiry: func(context.Context, Identity) (time.Time, error) {
			if *failing {
				return time.Time{}, errors.New("credential carries no expiresAt")
			}
			return expiry, nil
		},
		forceRefresh: func(context.Context, Identity) error { return nil },
	})
	if w == nil {
		t.Fatal("New returned nil for a non-empty identity set")
	}
	return w, c
}

// TestObserverIsEdgeTriggered is the whole reason the observer exists. The
// warden retries every RetryInterval, so on 2026-09-07 an unreadable credential
// produced 76 identical warnings in 38 minutes. Alerting on each one would have
// been 76 DMs; the admin must get one.
func TestObserverIsEdgeTriggered(t *testing.T) {
	failing := true
	w, c := observedWarden(t, &failing, base.Add(time.Hour))

	for i := 0; i < 10; i++ {
		w.checkOne(context.Background(), testID)
	}

	seen := c.snapshot()
	if len(seen) != 1 {
		t.Fatalf("got %d notifications for one continuous outage, want 1: %+v", len(seen), seen)
	}
	if !seen[0].Degraded {
		t.Error("the single notification should report the credential as degraded")
	}
	if seen[0].Reason == "" {
		t.Error("a degraded notification must carry the failure that caused it")
	}
}

// A single failed read is not a broken credential — the keychain lookup can lose
// to a locked machine. Nothing is reported until the second consecutive miss.
func TestObserverWaitsForASecondConsecutiveFailure(t *testing.T) {
	failing := true
	w, c := observedWarden(t, &failing, base.Add(time.Hour))

	w.checkOne(context.Background(), testID)
	if got := c.snapshot(); len(got) != 0 {
		t.Fatalf("reported after a single failure: %+v", got)
	}
	w.checkOne(context.Background(), testID)
	if got := c.snapshot(); len(got) != 1 {
		t.Fatalf("got %d notifications after two failures, want 1", len(got))
	}
}

// A failure that clears before the threshold must leave no trace: the counter
// resets, so a blip every other pass never accumulates into an alert.
func TestObserverResetsOnASuccessfulPass(t *testing.T) {
	failing := true
	w, c := observedWarden(t, &failing, base.Add(time.Hour))

	for i := 0; i < 6; i++ {
		w.checkOne(context.Background(), testID)
		failing = !failing
	}
	if got := c.snapshot(); len(got) != 0 {
		t.Fatalf("an alternating blip should never be reported: %+v", got)
	}
}

// Recovery closes the outage, and carries when it started so a report can say
// how long it lasted.
func TestObserverReportsRecovery(t *testing.T) {
	failing := true
	w, c := observedWarden(t, &failing, base.Add(time.Hour))

	w.checkOne(context.Background(), testID)
	w.checkOne(context.Background(), testID)
	failing = false
	w.checkOne(context.Background(), testID)

	seen := c.snapshot()
	if len(seen) != 2 {
		t.Fatalf("got %d notifications, want a degraded and a recovered: %+v", len(seen), seen)
	}
	if seen[1].Degraded {
		t.Error("the second notification should report recovery")
	}
	if seen[1].Since.IsZero() {
		t.Error("recovery must carry when the outage started, so a report can say how long it ran")
	}
	if seen[1].Reason != "" {
		t.Error("recovery carries no failure reason")
	}

	// And it re-arms: a later outage is reported again rather than swallowed.
	failing = true
	w.checkOne(context.Background(), testID)
	w.checkOne(context.Background(), testID)
	if got := c.snapshot(); len(got) != 3 {
		t.Fatalf("got %d notifications, want a second outage reported: %+v", len(got), got)
	}
}

// A refusal from the server is as much a lockout as an unreadable store, and
// reaches the admin the same way.
func TestObserverReportsARefusedRefresh(t *testing.T) {
	c := &collector{}
	w := New(Options{
		Identities:    []Identity{testID},
		DegradedAfter: 1,
		Observer:      c.observe,
		now:           func() time.Time { return base },
		readExpiry: func(context.Context, Identity) (time.Time, error) {
			// Inside the forcing window, so every pass tries a refresh.
			return base.Add(time.Minute), nil
		},
		forceRefresh: func(context.Context, Identity) error {
			return errors.New("OAuth session expired and could not be refreshed")
		},
	})
	w.checkOne(context.Background(), testID)

	seen := c.snapshot()
	if len(seen) != 1 || !seen[0].Degraded {
		t.Fatalf("a refused refresh should be reported as degraded, got %+v", seen)
	}
}

// A warden with no observer must behave exactly as before — the alerting is an
// addition, not a dependency.
func TestNoObserverIsHarmless(t *testing.T) {
	h := newHarness(t, base.Add(time.Hour))
	h.warden.checkOne(context.Background(), testID)
	// SetObserver on a nil warden is a no-op rather than a panic, so the wiring
	// can be unconditional at the call site.
	var nilWarden *Warden
	nilWarden.SetObserver(func(Health) {})
	nilWarden.observe(testID)
}

// SetObserver is how the gateway attaches one after construction, because the
// alerter needs the gateway and the gateway needs the warden.
func TestSetObserverAttachesAfterConstruction(t *testing.T) {
	c := &collector{}
	failing := true
	w, _ := observedWarden(t, &failing, base.Add(time.Hour))
	w.SetObserver(c.observe)

	w.checkOne(context.Background(), testID)
	w.checkOne(context.Background(), testID)
	if got := c.snapshot(); len(got) != 1 {
		t.Fatalf("got %d notifications through the late-attached observer, want 1", len(got))
	}
}

func TestHealthsSayWhereEachCredentialStandsNow(t *testing.T) {
	failing := true
	w, _ := observedWarden(t, &failing, base.Add(time.Hour))

	if got := w.Healths(); len(got) != 1 || got[0].Degraded || !got[0].ExpiresAt.IsZero() {
		t.Fatalf("before any pass: %+v, want one credential, not failing and not yet read", got)
	}
	w.checkOne(context.Background(), testID)
	w.checkOne(context.Background(), testID)
	got := w.Healths()
	if len(got) != 1 || !got[0].Degraded || got[0].Reason == "" || got[0].Since.IsZero() {
		t.Fatalf("mid-outage: %+v, want it failing, with the reason and since when", got)
	}
	failing = false
	w.checkOne(context.Background(), testID)
	if got := w.Healths(); got[0].Degraded || got[0].Reason != "" || !got[0].ExpiresAt.Equal(base.Add(time.Hour)) {
		t.Fatalf("after recovery: %+v, want it working with the expiry it read", got[0])
	}
}
