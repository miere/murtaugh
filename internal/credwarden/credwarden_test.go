package credwarden

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is a manually advanced clock so the scheduling can be driven without
// waiting on real time.
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

var (
	base   = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	testID = Identity{Command: "/usr/local/bin/claude"}
)

// harness wires a Warden onto fake seams. The refresh seam models the real CLI:
// it only moves the expiry when the credential is inside refreshWindow, which is
// exactly why exit status 0 cannot be trusted as success.
type harness struct {
	clock  *fakeClock
	warden *Warden

	mu            sync.Mutex
	expiry        time.Time
	forced        int
	readErr       error
	forceErr      error
	refreshWindow time.Duration // CLI refreshes only within this of expiry
	neverRefresh  bool          // model a credential that cannot be refreshed at all
	newTokenLife  time.Duration
	readErrAfter  int // fail the Nth read onward (0 = never); counts all reads
	reads         int
}

func newHarness(t *testing.T, expiry time.Time, opts ...func(*harness)) *harness {
	t.Helper()
	h := &harness{
		clock:         newClock(base),
		expiry:        expiry,
		refreshWindow: 5 * time.Minute, // the measured Claude Code threshold
		newTokenLife:  8 * time.Hour,
	}
	for _, o := range opts {
		o(h)
	}
	h.warden = New(Options{
		Identities: []Identity{testID},
		now:        h.clock.Now,
		readExpiry: func(context.Context, Identity) (time.Time, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.reads++
			if h.readErr != nil {
				return time.Time{}, h.readErr
			}
			if h.readErrAfter > 0 && h.reads >= h.readErrAfter {
				return time.Time{}, errors.New("keychain unavailable")
			}
			return h.expiry, nil
		},
		forceRefresh: func(context.Context, Identity) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.forced++
			if h.forceErr != nil {
				return h.forceErr
			}
			// The CLI refreshes only when the token is already close to expiry.
			if !h.neverRefresh && h.expiry.Sub(h.clock.Now()) <= h.refreshWindow {
				h.expiry = h.clock.Now().Add(h.newTokenLife)
			}
			return nil
		},
	})
	if h.warden == nil {
		t.Fatal("New returned nil for a non-empty identity set")
	}
	return h
}

func (h *harness) forcedCount() int { h.mu.Lock(); defer h.mu.Unlock(); return h.forced }
func (h *harness) currentExpiry() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.expiry
}

// run drives checkAll the way Run would, advancing the fake clock by whatever
// wait the warden asked for. Returns the waits, so the schedule itself can be
// asserted on.
func (h *harness) run(passes int) []time.Duration {
	var waits []time.Duration
	for i := 0; i < passes; i++ {
		w := h.warden.checkAll(context.Background())
		waits = append(waits, w)
		h.clock.Advance(w)
	}
	return waits
}

// The whole point of the change: with the credential hours out, the warden must
// sleep rather than spend calls, and must not overshoot the window.
func TestSleepsUntilTheWindowRatherThanPolling(t *testing.T) {
	h := newHarness(t, base.Add(8*time.Hour))

	waits := h.run(1)
	if got := h.forcedCount(); got != 0 {
		t.Fatalf("expected no forcing turn 8h out, got %d", got)
	}
	if waits[0] != DefaultMaxSleep {
		t.Fatalf("first wait = %v, want the %v cap", waits[0], DefaultMaxSleep)
	}
	// Waits are capped, so a suspended host re-reads the real expiry promptly
	// instead of trusting a monotonic timer that stopped while it was away.
	for _, w := range h.run(20) {
		if w > DefaultMaxSleep {
			t.Fatalf("wait %v exceeded the cap %v", w, DefaultMaxSleep)
		}
	}
	if got := h.forcedCount(); got != 0 {
		t.Fatalf("still 3h+ from expiry; expected no forcing turns, got %d", got)
	}
}

// The measured behaviour: a turn outside Claude Code's own 5-minute threshold
// exits 0 and changes nothing. The warden must not count that as a refresh.
func TestExitZeroIsNotSuccess(t *testing.T) {
	// 2 minutes out: inside our force window, and inside the CLI's too.
	h := newHarness(t, base.Add(2*time.Minute))
	h.run(1)

	if got := h.forcedCount(); got != 1 {
		t.Fatalf("expected one forcing turn, got %d", got)
	}
	st := h.warden.States()[0]
	if st.Refreshes != 1 {
		t.Fatalf("expected the refresh to be counted once it moved the expiry, got %d", st.Refreshes)
	}

	// Now the opposite: a turn that completes but moves nothing.
	h2 := newHarness(t, base.Add(2*time.Minute), func(h *harness) {
		h.neverRefresh = true
	})
	h2.run(1)
	if got := h2.forcedCount(); got != 1 {
		t.Fatalf("expected one forcing turn, got %d", got)
	}
	st2 := h2.warden.States()[0]
	if st2.Refreshes != 0 {
		t.Fatalf("a turn that did not move the expiry must not count as a refresh, got %d", st2.Refreshes)
	}
	if st2.Attempts != 1 {
		t.Fatalf("expected the fruitless attempt to be recorded, got %d", st2.Attempts)
	}
}

// The Sep-1 failure in miniature: the credential lapses while the warden is not
// looking. On the next pass it must act immediately rather than treat a negative
// remaining as "not due yet".
func TestActsOnAnAlreadyLapsedCredential(t *testing.T) {
	h := newHarness(t, base.Add(-2*time.Hour)) // expired two hours ago
	h.run(1)

	if got := h.forcedCount(); got != 1 {
		t.Fatalf("expected an immediate forcing turn for a lapsed credential, got %d", got)
	}
	if !h.currentExpiry().After(h.clock.Now()) {
		t.Fatal("expected the lapsed credential to have been refreshed forward")
	}
}

// A credential that cannot be refreshed must not burn an inference call every
// retry for as long as the daemon runs.
func TestBacksOffWhenTheExpiryNeverMoves(t *testing.T) {
	h := newHarness(t, base.Add(2*time.Minute), func(h *harness) {
		h.neverRefresh = true
	})

	waits := h.run(DefaultMaxAttemptsPerExpiry + 3)

	if got := h.forcedCount(); got != DefaultMaxAttemptsPerExpiry {
		t.Fatalf("expected attempts to stop at %d, got %d", DefaultMaxAttemptsPerExpiry, got)
	}
	// Once it gives up it must back off to the long wait, not keep retrying fast.
	if last := waits[len(waits)-1]; last != DefaultMaxSleep {
		t.Fatalf("expected a backed-off wait of %v, got %v", DefaultMaxSleep, last)
	}
}

// Aiming at 3 minutes has to land inside the CLI's 5-minute threshold — that is
// the entire premise. One turn should do it.
func TestOneTurnSufficesWhenAimed(t *testing.T) {
	h := newHarness(t, base.Add(90*time.Minute))

	// Sleep forward until the window opens, then act.
	for i := 0; i < 40 && h.forcedCount() == 0; i++ {
		h.run(1)
	}
	if got := h.forcedCount(); got != 1 {
		t.Fatalf("expected exactly one forcing turn, got %d", got)
	}
	st := h.warden.States()[0]
	if st.Refreshes != 1 {
		t.Fatalf("expected the aimed turn to refresh, got %d refreshes", st.Refreshes)
	}
	// And the turn must have happened inside the CLI's window, not before it.
	if st.Attempts != 0 {
		t.Fatalf("expected no wasted attempts, got %d", st.Attempts)
	}
}

func TestReadFailureIsRecordedAndRetried(t *testing.T) {
	h := newHarness(t, base.Add(time.Hour))
	h.mu.Lock()
	h.readErr = errors.New("keychain unavailable")
	h.mu.Unlock()

	waits := h.run(1)
	if got := h.forcedCount(); got != 0 {
		t.Fatalf("expected no forcing turn when the expiry is unknown, got %d", got)
	}
	if waits[0] != DefaultRetryInterval {
		t.Fatalf("expected a retry wait of %v, got %v", DefaultRetryInterval, waits[0])
	}
	if st := h.warden.States()[0]; st.LastError == "" {
		t.Fatal("expected the read failure to be recorded for the diagnostics surface")
	}
}

func TestRefreshFailureIsRecordedAndCounted(t *testing.T) {
	h := newHarness(t, base.Add(2*time.Minute))
	h.mu.Lock()
	h.forceErr = errors.New("exit status 1: not logged in")
	h.mu.Unlock()

	h.run(1)
	st := h.warden.States()[0]
	if st.Refreshes != 0 {
		t.Fatalf("a failed turn must not count as a refresh, got %d", st.Refreshes)
	}
	if st.Attempts != 1 || st.LastError == "" {
		t.Fatalf("expected the failure recorded and counted, got %+v", st)
	}
}

// If the verification read fails we cannot claim a refresh, but we also must not
// lose the credential to a transient keychain hiccup.
func TestVerificationFailureDoesNotClaimSuccess(t *testing.T) {
	h := newHarness(t, base.Add(2*time.Minute), func(h *harness) {
		h.readErrAfter = 2 // first read ok, the verify read fails
	})
	h.run(1)

	st := h.warden.States()[0]
	if st.Refreshes != 0 {
		t.Fatalf("an unverifiable turn must not count as a refresh, got %d", st.Refreshes)
	}
	if !strings.Contains(st.LastError, "verify") {
		t.Fatalf("expected the verification failure to be named, got %q", st.LastError)
	}
}

func TestNextCheckIsPublished(t *testing.T) {
	h := newHarness(t, base.Add(8*time.Hour))
	h.run(1)
	if st := h.warden.States()[0]; st.NextCheck.IsZero() {
		t.Fatal("expected the warden to publish when it next intends to look")
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
	if got[1].Home != "/home/other" {
		t.Fatalf("expected a HOME override to be a distinct credential, got %v", got[1])
	}
}

func TestNewReturnsNilWithNoIdentities(t *testing.T) {
	if w := New(Options{}); w != nil {
		t.Fatal("expected nil warden with no identities, so the caller skips starting it")
	}
	if w := New(Options{Identities: []Identity{{Command: "  "}}}); w != nil {
		t.Fatal("expected nil warden when the only identity has a blank command")
	}
}

// One unreadable credential must not starve the others.
func TestOneBadCredentialDoesNotBlockOthers(t *testing.T) {
	clock := newClock(base)
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
			return clock.Now().Add(2 * time.Minute), nil
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
	h := newHarness(t, base.Add(8*time.Hour))
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
	w.Run(context.Background())
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
	if strings.Contains(err.Error(), "sk-ant") {
		t.Fatalf("parse error leaked credential material: %q", err.Error())
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

func TestCapDuration(t *testing.T) {
	if got := capDuration(-time.Hour, time.Minute); got != 0 {
		t.Fatalf("a negative duration must clamp to zero, got %v", got)
	}
	if got := capDuration(time.Hour, time.Minute); got != time.Minute {
		t.Fatalf("capDuration = %v, want %v", got, time.Minute)
	}
}
