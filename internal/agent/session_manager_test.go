package agent

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClient struct {
	initialized atomic.Int32
	sessions    atomic.Int32
}

func (f *fakeClient) Initialize(context.Context) error {
	f.initialized.Add(1)
	return nil
}

func (f *fakeClient) NewSession(context.Context, SessionMetadata) (Session, error) {
	id := f.sessions.Add(1)
	return Session{ID: fmt.Sprintf("session-%d", id)}, nil
}

func (f *fakeClient) Prompt(context.Context, string, PromptRequest) (<-chan Event, error) {
	ch := make(chan Event, 1)
	ch <- Event{Type: EventComplete}
	close(ch)
	return ch, nil
}

func (f *fakeClient) Cancel(context.Context, string) error { return nil }
func (f *fakeClient) Close() error                         { return nil }

// closingClient implements the optional sessionCloser seam so eviction/discard
// can be asserted to release a backend's per-session resources.
type closingClient struct {
	fakeClient
	closed []string
}

func (c *closingClient) CloseSession(id string) { c.closed = append(c.closed, id) }

// runTurn drives one complete turn through the manager and drains it to close,
// which is both what a real caller does and — because endTurn runs before the
// forwarded channel closes — the point at which the session is provably idle
// again.
func runTurn(t *testing.T, m *SessionManager, key ConversationKey) {
	t.Helper()
	ch, err := m.Prompt(context.Background(), key, SessionMetadata{}, PromptRequest{Text: "hi"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	for range ch {
	}
}

func TestSessionManagerClosesClientSessionOnDiscard(t *testing.T) {
	c := &closingClient{}
	m := NewSessionManager(c, time.Hour, 100)
	key := ConversationKey{ChannelID: "C", ThreadTS: "1"}
	runTurn(t, m, key)
	id, ok := m.Lookup(key)
	if !ok {
		t.Fatal("expected a live session after the turn")
	}
	m.Discard(key)
	if len(c.closed) != 1 || c.closed[0] != id {
		t.Fatalf("Discard should CloseSession %q, got %v", id, c.closed)
	}
}

func TestSessionManagerClosesClientSessionOnEvict(t *testing.T) {
	c := &closingClient{}
	now := time.Now()
	m := NewSessionManager(c, time.Minute, 100)
	m.now = func() time.Time { return now }
	first := ConversationKey{ChannelID: "C", ThreadTS: "1"}
	runTurn(t, m, first)
	id, _ := m.Lookup(first)
	// Advance past the idle timeout; opening another session triggers evictLocked.
	now = now.Add(2 * time.Minute)
	runTurn(t, m, ConversationKey{ChannelID: "C", ThreadTS: "2"})
	found := false
	for _, closed := range c.closed {
		if closed == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("idle eviction should CloseSession %q, got %v", id, c.closed)
	}
}

// blockingClient hands out prompt channels the test closes by hand, so a turn
// can be held open across an eviction sweep — the situation the manager used to
// get wrong.
type blockingClient struct {
	fakeClient
	mu     sync.Mutex
	closed []string
	open   []chan Event
}

func (b *blockingClient) Prompt(context.Context, string, PromptRequest) (<-chan Event, error) {
	ch := make(chan Event)
	b.mu.Lock()
	b.open = append(b.open, ch)
	b.mu.Unlock()
	return ch, nil
}

func (b *blockingClient) CloseSession(id string) {
	b.mu.Lock()
	b.closed = append(b.closed, id)
	b.mu.Unlock()
}

func (b *blockingClient) closedIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.closed...)
}

// beginTurn starts a turn and leaves it in flight. The returned finish closes the
// agent's side and waits for the manager to release the session.
func beginTurn(t *testing.T, m *SessionManager, c *blockingClient, key ConversationKey) (string, func()) {
	t.Helper()
	events, err := m.Prompt(context.Background(), key, SessionMetadata{}, PromptRequest{Text: "hi"})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	id, ok := m.Lookup(key)
	if !ok {
		t.Fatal("expected a live session for the in-flight turn")
	}
	c.mu.Lock()
	in := c.open[len(c.open)-1]
	c.mu.Unlock()
	return id, func() {
		close(in)
		for range events {
		}
	}
}

// cycleTurn runs one whole turn against a blockingClient. Tests use it for the
// turn that merely triggers a sweep, as opposed to the one held open across it.
func cycleTurn(t *testing.T, m *SessionManager, c *blockingClient, key ConversationKey) {
	t.Helper()
	_, finish := beginTurn(t, m, c, key)
	finish()
}

// The regression test for the bug this guard exists to stop: a conversation with
// a turn in flight is NOT idle, however long the turn has been running, and a new
// conversation elsewhere must not sweep it.
func TestSessionManagerDoesNotEvictSessionRunningATurn(t *testing.T) {
	c := &blockingClient{}
	now := time.Now()
	m := NewSessionManager(c, time.Minute, 100)
	m.now = func() time.Time { return now }
	working := ConversationKey{ChannelID: "C", ThreadTS: "1"}
	id, finish := beginTurn(t, m, c, working)

	// Well past the idle timeout — but the turn is still running.
	now = now.Add(time.Hour)
	cycleTurn(t, m, c, ConversationKey{ChannelID: "C", ThreadTS: "2"})

	for _, closed := range c.closedIDs() {
		if closed == id {
			t.Fatalf("a session with a turn in flight was evicted as idle (%q)", id)
		}
	}
	if _, live := m.Lookup(working); !live {
		t.Fatal("the working conversation lost its session to the idle sweep")
	}
	finish()
}

// A turn that ran for hours must get a full idle window once it ends, rather
// than being sweepable the instant it falls quiet.
func TestSessionManagerIdleClockRestartsWhenTurnEnds(t *testing.T) {
	c := &blockingClient{}
	now := time.Now()
	m := NewSessionManager(c, time.Minute, 100)
	m.now = func() time.Time { return now }
	working := ConversationKey{ChannelID: "C", ThreadTS: "1"}
	id, finish := beginTurn(t, m, c, working)

	now = now.Add(3 * time.Hour)
	finish()

	// Thirty seconds after a three-hour turn: still inside the idle window.
	now = now.Add(30 * time.Second)
	cycleTurn(t, m, c, ConversationKey{ChannelID: "C", ThreadTS: "2"})
	for _, closed := range c.closedIDs() {
		if closed == id {
			t.Fatalf("session %q was swept 30s after a long turn ended; the idle clock did not restart", id)
		}
	}

	// Two minutes later it is genuinely idle and goes.
	now = now.Add(2 * time.Minute)
	cycleTurn(t, m, c, ConversationKey{ChannelID: "C", ThreadTS: "3"})
	if !contains(c.closedIDs(), id) {
		t.Fatalf("session %q survived a real idle window, closed=%v", id, c.closedIDs())
	}
}

// The runaway guard: work is expected to be long, but not endless.
func TestSessionManagerEvictsWedgedSessionOnBusyTimeout(t *testing.T) {
	c := &blockingClient{}
	now := time.Now()
	m := NewSessionManager(c, time.Minute, 100).WithBusyTimeout(18 * time.Hour)
	m.now = func() time.Time { return now }
	id, finish := beginTurn(t, m, c, ConversationKey{ChannelID: "C", ThreadTS: "1"})

	// Seventeen hours in, it is still working as far as the manager is concerned.
	now = now.Add(17 * time.Hour)
	cycleTurn(t, m, c, ConversationKey{ChannelID: "C", ThreadTS: "2"})
	if contains(c.closedIDs(), id) {
		t.Fatalf("session %q was reaped at 17h, inside the 18h busy timeout", id)
	}

	now = now.Add(2 * time.Hour)
	cycleTurn(t, m, c, ConversationKey{ChannelID: "C", ThreadTS: "3"})
	if !contains(c.closedIDs(), id) {
		t.Fatalf("session %q survived 19h of one turn, closed=%v", id, c.closedIDs())
	}
	finish()
}

// At capacity with every slot working, the new conversation is refused rather
// than served by killing somebody else's turn.
func TestSessionManagerRefusesNewConversationWhenEverySlotIsBusy(t *testing.T) {
	c := &blockingClient{}
	m := NewSessionManager(c, time.Hour, 2)
	var finishers []func()
	for _, thread := range []string{"1", "2"} {
		_, finish := beginTurn(t, m, c, ConversationKey{ChannelID: "C", ThreadTS: thread})
		finishers = append(finishers, finish)
	}

	_, err := m.Prompt(context.Background(), ConversationKey{ChannelID: "C", ThreadTS: "3"}, SessionMetadata{}, PromptRequest{Text: "hi"})
	capacity, ok := AtCapacity(err)
	if !ok {
		t.Fatalf("expected a CapacityError once every slot was busy, got %v", err)
	}
	if capacity.Limit != 2 {
		t.Fatalf("CapacityError should carry the configured limit, got %d", capacity.Limit)
	}
	if closed := c.closedIDs(); len(closed) != 0 {
		t.Fatalf("a busy session was evicted to make room: %v", closed)
	}
	for _, finish := range finishers {
		finish()
	}
}

// At capacity with something idle, the idle one goes and the new conversation is
// served — the refusal is a last resort, not the first answer.
func TestSessionManagerCapacityTakesTheIdleSessionNotTheBusyOne(t *testing.T) {
	c := &blockingClient{}
	now := time.Now()
	m := NewSessionManager(c, time.Hour, 2)
	m.now = func() time.Time { return now }

	quiet := ConversationKey{ChannelID: "C", ThreadTS: "1"}
	_, finishQuiet := beginTurn(t, m, c, quiet)
	finishQuiet()
	quietID, _ := m.Lookup(quiet)

	now = now.Add(time.Second)
	busyID, finishBusy := beginTurn(t, m, c, ConversationKey{ChannelID: "C", ThreadTS: "2"})

	// Still inside the idle timeout, so only the capacity squeeze can free a slot.
	now = now.Add(time.Second)
	if _, err := m.Prompt(context.Background(), ConversationKey{ChannelID: "C", ThreadTS: "3"}, SessionMetadata{}, PromptRequest{Text: "hi"}); err != nil {
		t.Fatalf("expected the idle session to be evicted for room, got %v", err)
	}
	closed := c.closedIDs()
	if !contains(closed, quietID) {
		t.Fatalf("capacity should have taken the idle session %q, closed=%v", quietID, closed)
	}
	if contains(closed, busyID) {
		t.Fatalf("capacity took the working session %q, closed=%v", busyID, closed)
	}
	finishBusy()
}

func TestSessionManagerReportsEvictionsToObserver(t *testing.T) {
	c := &closingClient{}
	now := time.Now()
	var seen []Eviction
	m := NewSessionManager(c, time.Minute, 100).WithEvictionObserver(func(e Eviction) { seen = append(seen, e) })
	m.now = func() time.Time { return now }
	runTurn(t, m, ConversationKey{ChannelID: "C", ThreadTS: "1"})

	now = now.Add(90 * time.Second)
	runTurn(t, m, ConversationKey{ChannelID: "C", ThreadTS: "2"})

	if len(seen) != 1 {
		t.Fatalf("expected one reported eviction, got %d", len(seen))
	}
	if seen[0].Reason != EvictionIdle {
		t.Fatalf("expected reason %q, got %q", EvictionIdle, seen[0].Reason)
	}
	if seen[0].Age != 90*time.Second {
		t.Fatalf("expected the eviction to report a 90s age, got %s", seen[0].Age)
	}
	if seen[0].Key.ThreadTS != "1" {
		t.Fatalf("expected the evicted conversation's key, got %+v", seen[0].Key)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// probingClient adds the optional SupportsCancel capability surface so the
// manager's auto-detection path can be exercised.
type probingClient struct {
	fakeClient
	supports bool
	probes   atomic.Int32
}

func (p *probingClient) SupportsCancel(context.Context) bool {
	p.probes.Add(1)
	return p.supports
}

func boolPtr(b bool) *bool { return &b }

func TestSessionManagerInterruptibleDefaultsTrueBeforeWarm(t *testing.T) {
	m := NewSessionManager(&fakeClient{}, time.Minute, 10)
	if !m.Interruptible() {
		t.Fatal("expected unknown interruptibility to default to true before Warm")
	}
}

func TestSessionManagerProbeDetectsInterruptibility(t *testing.T) {
	for _, tc := range []struct {
		name     string
		supports bool
	}{
		{"supported", true},
		{"unsupported", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &probingClient{supports: tc.supports}
			m := NewSessionManager(client, time.Minute, 10)
			if err := m.Warm(context.Background()); err != nil {
				t.Fatalf("Warm returned error: %v", err)
			}
			if client.probes.Load() != 1 {
				t.Fatalf("expected exactly one capability probe, got %d", client.probes.Load())
			}
			if m.Interruptible() != tc.supports {
				t.Fatalf("Interruptible() = %v, want %v", m.Interruptible(), tc.supports)
			}
		})
	}
}

func TestSessionManagerCancelOverrideSkipsProbe(t *testing.T) {
	client := &probingClient{supports: true} // probe would say true...
	m := NewSessionManager(client, time.Minute, 10).WithCancelOverride(boolPtr(false))
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("Warm returned error: %v", err)
	}
	if client.probes.Load() != 0 {
		t.Fatal("expected the config override to skip the probe")
	}
	if m.Interruptible() {
		t.Fatal("expected the config override (false) to win over the probe")
	}
}

func TestSessionManagerReusesSessionForConversationKey(t *testing.T) {
	client := &fakeClient{}
	manager := NewSessionManager(client, time.Hour, 10)
	key := ConversationKey{TeamID: "T1", ChannelID: "C1", ThreadTS: "123.4"}

	for i := 0; i < 2; i++ {
		ch, err := manager.Prompt(context.Background(), key, SessionMetadata{}, PromptRequest{Text: "hi"})
		if err != nil {
			t.Fatalf("Prompt returned error: %v", err)
		}
		for range ch {
		}
	}
	if client.initialized.Load() != 1 || client.sessions.Load() != 1 {
		t.Fatalf("expected one initialized client/session, got init=%d sessions=%d", client.initialized.Load(), client.sessions.Load())
	}
}

func TestSessionManagerCreatesDistinctSessionsForDistinctThreads(t *testing.T) {
	client := &fakeClient{}
	manager := NewSessionManager(client, time.Hour, 10)
	keys := []ConversationKey{{TeamID: "T1", ChannelID: "C1", ThreadTS: "1"}, {TeamID: "T1", ChannelID: "C1", ThreadTS: "2"}}
	for _, key := range keys {
		ch, err := manager.Prompt(context.Background(), key, SessionMetadata{}, PromptRequest{Text: "hi"})
		if err != nil {
			t.Fatalf("Prompt returned error: %v", err)
		}
		for range ch {
		}
	}
	if client.sessions.Load() != 2 {
		t.Fatalf("expected two sessions, got %d", client.sessions.Load())
	}
}

func TestSessionManagerDiscardForcesFreshSession(t *testing.T) {
	client := &fakeClient{}
	manager := NewSessionManager(client, time.Hour, 10)
	key := ConversationKey{TeamID: "T1", ChannelID: "C1", ThreadTS: "123.4"}

	prompt := func() {
		ch, err := manager.Prompt(context.Background(), key, SessionMetadata{}, PromptRequest{Text: "hi"})
		if err != nil {
			t.Fatalf("Prompt returned error: %v", err)
		}
		for range ch {
		}
	}

	prompt()
	// After discarding the wedged session, the next prompt for the same
	// conversation must open a brand-new session rather than reuse the old one.
	manager.Discard(key)
	prompt()

	if client.sessions.Load() != 2 {
		t.Fatalf("expected a fresh session after Discard, got %d sessions", client.sessions.Load())
	}
}
