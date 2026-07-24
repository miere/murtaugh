package agent

import (
	"context"
	"fmt"
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

func TestSessionManagerClosesClientSessionOnDiscard(t *testing.T) {
	c := &closingClient{}
	m := NewSessionManager(c, time.Hour, 100)
	key := ConversationKey{ChannelID: "C", ThreadTS: "1"}
	sess, err := m.session(context.Background(), key, SessionMetadata{})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	m.Discard(key)
	if len(c.closed) != 1 || c.closed[0] != sess.ID {
		t.Fatalf("Discard should CloseSession %q, got %v", sess.ID, c.closed)
	}
}

func TestSessionManagerClosesClientSessionOnEvict(t *testing.T) {
	c := &closingClient{}
	now := time.Now()
	m := NewSessionManager(c, time.Minute, 100)
	m.now = func() time.Time { return now }
	first := ConversationKey{ChannelID: "C", ThreadTS: "1"}
	sess, err := m.session(context.Background(), first, SessionMetadata{})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	// Advance past the idle timeout; opening another session triggers evictLocked.
	now = now.Add(2 * time.Minute)
	if _, err := m.session(context.Background(), ConversationKey{ChannelID: "C", ThreadTS: "2"}, SessionMetadata{}); err != nil {
		t.Fatalf("second session: %v", err)
	}
	found := false
	for _, id := range c.closed {
		if id == sess.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("idle eviction should CloseSession %q, got %v", sess.ID, c.closed)
	}
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
