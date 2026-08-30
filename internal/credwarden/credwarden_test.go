package credwarden

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock so the margin logic can be driven
// without waiting on real time.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock(t time.Time) *fakeClock { return &fakeClock{now: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// harness wires a Warden onto fake seams and counts what it did.
type harness struct {
	clock   *fakeClock
	warden  *Warden
	mu      sync.Mutex
	forced  int
	expiry  time.Time
	readErr error
	forceFn func() error
}

func newHarness(t *testing.T, id Identity, expiry time.Time) *harness {
	t.Helper()
	clock := newClock(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
	h := &harness{clock: clock, expiry: expiry}
	h.warden = New(Options{
		Identities: []Identity{id},
		now:        clock.Now,
		readExpiry: func(context.Context, Identity) (time.Time, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.readErr != nil {
				return time.Time{}, h.readErr
			}
			return h.expiry, nil
		},
		forceRefresh: func(context.Context, Identity) error {
			h.mu.Lock()
			h.forced++
			fn := h.forceFn
			h.mu.Unlock()
			if fn != nil {
				return fn()
			}
			return nil
		},
	})
	if h.warden == nil {
		t.Fatal("New returned nil for a non-empty identity set")
	}
	return h
}

func (h *harness) forcedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.forced
}

func (h *harness) setExpiry(t time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expiry = t
}

var testID = Identity{Command: "/usr/local/bin/claude"}

func TestNewReturnsNilWithNoIdentities(t *testing.T) {
	if w := New(Options{}); w != nil {
		t.Fatal("expected nil warden with no identities, so the caller skips starting it")
	}
	if w := New(Options{Identities: []Identity{{Command: "  "}}}); w != nil {
		t.Fatal("expected nil warden when the only identity has a blank command")
	}
}

func TestDedupeCollapsesSharedCredentials(t *testing.T) {
	w := New(Options{Identities: []Identity{
		{Command: "/bin/claude"},
		{Command: "/bin/claude"},
		{Command: "/bin/claude", Home: "/home/other"},
	}})
	got := w.Identities()
	if len(got) != 2 {
		t.Fatalf("expected 2 distinct credentials, got %d: %v", len(got), got)
	}
	// N claude_code profiles sharing one credential must yield ONE watcher:
	// concurrent refreshes race the server's token rotation.
	if got[0] != (Identity{Command: "/bin/claude"}) {
		t.Fatalf("expected first-seen order preserved, got %v", got[0])
	}
	if got[1].Home != "/home/other" {
		t.Fatalf("expected a HOME override to be a distinct credential, got %v", got[1])
	}
}

func TestNoRefreshWhileOutsideMargin(t *testing.T) {
	h := newHarness(t, testID, time.Date(2026, 8, 30, 20, 0, 0, 0, time.UTC)) // 8h out
	h.warden.checkAll(context.Background())

	if got := h.forcedCount(); got != 0 {
		t.Fatalf("expected no forcing turn 8h before expiry, got %d", got)
	}
	states := h.warden.States()
	if len(states) != 1 || states[0].ExpiresAt.IsZero() {
		t.Fatalf("expected the observed expiry to be recorded, got %+v", states)
	}
}

func TestRefreshFiresInsideMargin(t *testing.T) {
	// Expiry 5 minutes out, margin 15 → inside.
	h := newHarness(t, testID, h12(5*time.Minute))
	h.warden.checkAll(context.Background())

	if got := h.forcedCount(); got != 1 {
		t.Fatalf("expected exactly one forcing turn inside the margin, got %d", got)
	}
	st := h.warden.States()[0]
	if st.Refreshes != 1 || st.LastRefresh.IsZero() {
		t.Fatalf("expected a successful refresh to be recorded, got %+v", st)
	}
	if st.LastError != "" {
		t.Fatalf("expected no error after a clean refresh, got %q", st.LastError)
	}
}

// The warden cannot see Claude Code's own proactive-refresh threshold, so it
// re-attempts each tick while inside the margin. It must stop as soon as the
// observed expiry moves forward — otherwise it would keep spending inference
// calls for the whole life of the new token.
func TestRetriesUntilExpiryAdvancesThenStops(t *testing.T) {
	h := newHarness(t, testID, h12(5*time.Minute))
	ctx := context.Background()

	h.warden.checkAll(ctx) // attempt 1
	h.clock.Advance(minForceInterval + time.Second)
	h.warden.checkAll(ctx) // attempt 2 — expiry has not moved
	if got := h.forcedCount(); got != 2 {
		t.Fatalf("expected a second attempt while the expiry was unchanged, got %d", got)
	}

	// Claude Code finally refreshed: expiry jumps 8h into the future.
	h.setExpiry(h.clock.Now().Add(8 * time.Hour))
	h.clock.Advance(minForceInterval + time.Second)
	h.warden.checkAll(ctx)
	h.clock.Advance(minForceInterval + time.Second)
	h.warden.checkAll(ctx)

	if got := h.forcedCount(); got != 2 {
		t.Fatalf("expected no further attempts once the expiry advanced, got %d", got)
	}
}

// A credential that can no longer be refreshed (revoked refresh token) would
// otherwise burn one inference call per tick for as long as the daemon runs.
func TestMinForceIntervalThrottlesRepeatedAttempts(t *testing.T) {
	h := newHarness(t, testID, h12(5*time.Minute))
	ctx := context.Background()

	h.warden.checkAll(ctx)
	h.clock.Advance(minForceInterval / 3)
	h.warden.checkAll(ctx)
	h.clock.Advance(minForceInterval / 3)
	h.warden.checkAll(ctx)

	if got := h.forcedCount(); got != 1 {
		t.Fatalf("expected the floor to suppress rapid re-attempts, got %d", got)
	}
}

func TestReadErrorIsRecordedAndDoesNotForce(t *testing.T) {
	h := newHarness(t, testID, h12(5*time.Minute))
	h.mu.Lock()
	h.readErr = errors.New("keychain unavailable")
	h.mu.Unlock()

	h.warden.checkAll(context.Background())

	if got := h.forcedCount(); got != 0 {
		t.Fatalf("expected no forcing turn when the expiry is unknown, got %d", got)
	}
	st := h.warden.States()[0]
	if st.LastError == "" {
		t.Fatal("expected the read failure to be recorded for the diagnostics surface")
	}
}

func TestRefreshFailureIsRecordedAndSurvives(t *testing.T) {
	h := newHarness(t, testID, h12(5*time.Minute))
	h.mu.Lock()
	h.forceFn = func() error { return errors.New("exit status 1: not logged in") }
	h.mu.Unlock()

	h.warden.checkAll(context.Background())

	st := h.warden.States()[0]
	if st.Refreshes != 0 {
		t.Fatalf("expected a failed turn not to count as a refresh, got %d", st.Refreshes)
	}
	if st.LastError == "" {
		t.Fatal("expected the refresh failure to be recorded")
	}
}

// One unreadable credential must not starve the others.
func TestOneBadCredentialDoesNotBlockOthers(t *testing.T) {
	clock := newClock(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC))
	bad := Identity{Command: "/bin/claude", Home: "/bad"}
	good := Identity{Command: "/bin/claude", Home: "/good"}
	var forced int
	var mu sync.Mutex

	w := New(Options{
		Identities: []Identity{bad, good},
		now:        clock.Now,
		readExpiry: func(_ context.Context, id Identity) (time.Time, error) {
			if id == bad {
				return time.Time{}, errors.New("nope")
			}
			return clock.Now().Add(5 * time.Minute), nil
		},
		forceRefresh: func(_ context.Context, id Identity) error {
			mu.Lock()
			defer mu.Unlock()
			if id == good {
				forced++
			}
			return nil
		},
	})
	w.checkAll(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if forced != 1 {
		t.Fatalf("expected the healthy credential to still be refreshed, got %d", forced)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	h := newHarness(t, testID, h12(8*time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.warden.Run(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

func TestNilWardenIsInert(t *testing.T) {
	var w *Warden
	w.Run(context.Background()) // must not panic
	if got := w.States(); got != nil {
		t.Fatalf("expected nil states from a nil warden, got %v", got)
	}
	if got := w.Identities(); got != nil {
		t.Fatalf("expected nil identities from a nil warden, got %v", got)
	}
}

func TestParseExpiryReadsMilliseconds(t *testing.T) {
	got, err := parseExpiry([]byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-x","refreshToken":"sk-ant-ort01-y","expiresAt":1788034769330}}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := time.UnixMilli(1788034769330); !got.Equal(want) {
		t.Fatalf("expiry = %v, want %v", got, want)
	}
}

func TestParseExpiryRejectsBlobWithoutExpiry(t *testing.T) {
	for name, in := range map[string]string{
		"empty object": `{}`,
		"no expiresAt": `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-x"}}`,
		"zero":         `{"claudeAiOauth":{"expiresAt":0}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseExpiry([]byte(in)); err == nil {
				t.Fatal("expected an error rather than a zero expiry read as 1970")
			}
		})
	}
}

// A parse failure must not quote the input: it is a credential.
func TestParseExpiryErrorDoesNotEchoInput(t *testing.T) {
	_, err := parseExpiry([]byte(`{"claudeAiOauth": NOTJSON sk-ant-ort01-secret}`))
	if err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
	if got := err.Error(); contains(got, "sk-ant") {
		t.Fatalf("parse error leaked credential material: %q", got)
	}
}

func TestIdentityString(t *testing.T) {
	if got := (Identity{Command: "/bin/claude"}).String(); got != "/bin/claude" {
		t.Fatalf("String() = %q", got)
	}
	if got := (Identity{Command: "/bin/claude", Home: "/h"}).String(); got != "/bin/claude (HOME=/h)" {
		t.Fatalf("String() = %q", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// h12 returns a time d after the harness's fixed start instant.
func h12(d time.Duration) time.Time {
	return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC).Add(d)
}
