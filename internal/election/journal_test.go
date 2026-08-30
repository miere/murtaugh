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
	"github.com/miere/murtaugh/internal/journal"
)

// recordingJournal captures events so a test can assert what an operator would
// find after a failover.
type recordingJournal struct {
	mu     sync.Mutex
	events []journal.Event
}

func (r *recordingJournal) Record(_ context.Context, e journal.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordingJournal) snapshot() []journal.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]journal.Event(nil), r.events...)
}

// states returns the `state` payload field of every recorded event, in order —
// which is the sequence an operator reconstructs a failover from.
func (r *recordingJournal) states() []string {
	var out []string
	for _, e := range r.snapshot() {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			continue
		}
		state, _ := payload["state"].(string)
		out = append(out, state)
	}
	return out
}

// journalRunner builds a runner wired to a recording journal.
func journalRunner(t *testing.T, locker config.Locker, clock Clock) (*Runner, *recordingJournal) {
	t.Helper()
	rec := &recordingJournal{}
	r, err := New(Options{
		Locker:   locker,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:    clock,
		Recorder: rec,
		Election: config.ElectionConfig{LeaseSeconds: 30, RenewSeconds: 10},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, rec
}

// TestJournalRecordsAPromotion covers the ordinary event, and the field that
// makes a multi-node sequence orderable at all.
func TestJournalRecordsAPromotion(t *testing.T) {
	locker, clock := newFakeLocker(), newFakeClock()
	r, rec := journalRunner(t, locker, clock)

	r.step(context.Background())

	events := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("%d events recorded, want 1: %+v", len(events), events)
	}
	e := events[0]
	if e.Stream != journal.StreamGateway {
		t.Errorf("stream = %q, want %q", e.Stream, journal.StreamGateway)
	}
	if e.Kind != electionKind {
		t.Errorf("kind = %q, want %q", e.Kind, electionKind)
	}
	if e.Level != journal.LevelInfo {
		t.Errorf("level = %q, want info for an ordinary promotion", e.Level)
	}
	payload, ok := e.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload is %T, want a map", e.Payload)
	}
	// The epoch is the whole point: two nodes' logs interleave arbitrarily,
	// their epochs do not.
	if payload["epoch"] == nil {
		t.Error("promotion event carries no epoch; a sequence across nodes cannot be ordered")
	}
	if payload["backend"] == nil {
		t.Error("promotion event does not say which lock backend was used")
	}
}

// TestJournalRecordsTheCredentialExpiryStory is the case this exists for.
//
// A node whose store credentials lapse stands down correctly and silently: the
// outbound gate shuts before it could say anything in Slack, and the successor
// knows a lease expired but not why. The journal is the only place the sequence
// survives, so it has to carry the failure AND the stand-down, with the
// provider's own error attached.
func TestJournalRecordsTheCredentialExpiryStory(t *testing.T) {
	locker, clock := newFakeLocker(), newFakeClock()
	r, rec := journalRunner(t, locker, clock)
	ctx := context.Background()

	r.step(ctx)
	locker.set(func(l *fakeLocker) { l.renewErr = errors.New("rpc error: code = Unauthenticated") })

	// Renewals fail; the node holds on, then stands down before the lease
	// would lapse.
	clock.advance(10 * time.Second)
	r.step(ctx)
	clock.advance(15 * time.Second)
	r.step(ctx)

	states := rec.states()
	want := []string{"promoted", "renew_failed", "renew_failed", "stood_down"}
	if len(states) != len(want) {
		t.Fatalf("recorded %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("recorded %v, want %v", states, want)
		}
	}

	// The provider's own words have to survive: "Unauthenticated" and
	// "no such host" send the operator to different places.
	var sawError bool
	for _, e := range rec.snapshot() {
		payload, _ := e.Payload.(map[string]any)
		if msg, ok := payload["error"].(string); ok && msg != "" {
			sawError = true
		}
	}
	if !sawError {
		t.Error("no event carried the underlying error; the journal cannot explain the failover")
	}
}

// TestJournalRecordsAnUnreachableLock covers the standby half: a node that
// cannot read the lock never promotes, and without this it would do so in
// complete silence.
func TestJournalRecordsAnUnreachableLock(t *testing.T) {
	locker, clock := newFakeLocker(), newFakeClock()
	locker.set(func(l *fakeLocker) { l.acquireErr = errors.New("permission denied") })
	r, rec := journalRunner(t, locker, clock)

	r.step(context.Background())

	states := rec.states()
	if len(states) != 1 || states[0] != "lock_unreachable" {
		t.Fatalf("recorded %v, want a single lock_unreachable", states)
	}
	if rec.snapshot()[0].Level != journal.LevelWarn {
		t.Errorf("level = %q, want warn", rec.snapshot()[0].Level)
	}
}

// TestJournalIsQuietWhenNothingHappens keeps the stream readable. The runner
// ticks every few seconds forever; recording each uneventful renewal would
// bury the events that matter.
func TestJournalIsQuietWhenNothingHappens(t *testing.T) {
	locker, clock := newFakeLocker(), newFakeClock()
	r, rec := journalRunner(t, locker, clock)
	ctx := context.Background()

	r.step(ctx) // promotion: one event
	for i := 0; i < 20; i++ {
		clock.advance(10 * time.Second)
		r.step(ctx)
	}

	if got := len(rec.snapshot()); got != 1 {
		t.Errorf("%d events for one promotion and twenty healthy renewals, want 1", got)
	}
}

// TestNilRecorderIsSafe covers CLI/MCP builds and struct-literal tests, which
// wire no journal at all.
func TestNilRecorderIsSafe(t *testing.T) {
	locker, clock := newFakeLocker(), newFakeClock()
	r, err := New(Options{
		Locker:   locker,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Clock:    clock,
		Election: config.ElectionConfig{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.step(context.Background())
	if !r.Leading() {
		t.Error("a runner with no recorder did not promote")
	}
}
