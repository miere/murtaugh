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

// brokerClient stands in for the node broker: a session id it did not mint is
// ErrSessionGone, which is what a runtime node disconnecting looks like from
// the gateway's side.
type brokerClient struct {
	fakeClient
	// live is the only session id this client will answer for. Every session it
	// mints becomes the live one; the previous node's id therefore stops
	// resolving the moment a new session is opened.
	live          string
	prompted      []string
	conversations []ConversationKey
}

func (b *brokerClient) NewSession(ctx context.Context, meta SessionMetadata) (Session, error) {
	if key, ok := ConversationFromContext(ctx); ok {
		b.conversations = append(b.conversations, key)
	}
	session, err := b.fakeClient.NewSession(ctx, meta)
	b.live = session.ID
	return session, err
}

func (b *brokerClient) Prompt(ctx context.Context, sessionID string, req PromptRequest) (<-chan Event, error) {
	b.prompted = append(b.prompted, sessionID)
	if sessionID != b.live {
		return nil, ErrSessionGone
	}
	return b.fakeClient.Prompt(ctx, sessionID, req)
}

// TestPromptOpensANewSessionWhenTheOldOneIsGone is the recovery half of #196's
// re-election. Overwriting the stored pin is not enough on its own: this map
// still binds the conversation to the dead node's session id, and handing that
// id to the node that took over produces an error the user sees on every turn.
func TestPromptOpensANewSessionWhenTheOldOneIsGone(t *testing.T) {
	c := &brokerClient{}
	m := NewSessionManager(c, time.Hour, 100)
	key := ConversationKey{ChannelID: "C", ThreadTS: "1", DM: true}
	ctx := context.Background()

	if _, err := m.Prompt(ctx, key, SessionMetadata{ChannelID: "C"}, PromptRequest{Text: "one"}); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	stale := c.live
	// The node holding it disconnects. Nothing tells the manager; it finds out
	// by being refused.
	c.live = "gone"

	if _, err := m.Prompt(ctx, key, SessionMetadata{ChannelID: "C"}, PromptRequest{Text: "two"}); err != nil {
		t.Fatalf("the turn after the node left was not recovered: %v", err)
	}
	if len(c.prompted) != 3 || c.prompted[1] != stale || c.prompted[2] == stale {
		t.Fatalf("expected a refused prompt on %q followed by a fresh session; got %v", stale, c.prompted)
	}
	if id, ok := m.Lookup(key); !ok || id == stale {
		t.Fatalf("the conversation is still bound to the dead node's session: %q %v", id, ok)
	}
}

// TestPromptDoesNotRetryForever bounds the recovery at one attempt: a second
// refusal is a genuine failure and is reported rather than looped on.
func TestPromptDoesNotRetryForever(t *testing.T) {
	c := &alwaysGoneClient{}
	m := NewSessionManager(c, time.Hour, 100)
	key := ConversationKey{ChannelID: "C", ThreadTS: "1"}

	_, err := m.Prompt(context.Background(), key, SessionMetadata{}, PromptRequest{Text: "one"})
	if err == nil {
		t.Fatal("a session that can never be opened reported success")
	}
	// Two prompts, not a loop: the first session's and the replacement's.
	if len(c.prompted) != 2 {
		t.Fatalf("expected exactly two attempts, got %v", c.prompted)
	}
}

// alwaysGoneClient refuses every prompt, however fresh the session.
type alwaysGoneClient struct {
	fakeClient
	prompted []string
}

func (a *alwaysGoneClient) Prompt(context.Context, string, PromptRequest) (<-chan Event, error) {
	a.prompted = append(a.prompted, "refused")
	return nil, ErrSessionGone
}

// TestPromptCarriesTheConversationToTheClient covers the plumbing delegation
// depends on: SessionMetadata has no DM flag, so the key is the only thing that
// can tell a DM thread from a channel thread with the same ids.
func TestPromptCarriesTheConversationToTheClient(t *testing.T) {
	c := &brokerClient{}
	m := NewSessionManager(c, time.Hour, 100)
	key := ConversationKey{TeamID: "T", ChannelID: "C", ThreadTS: "1", DM: true}

	if _, err := m.Prompt(context.Background(), key, SessionMetadata{ChannelID: "C"}, PromptRequest{Text: "one"}); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if len(c.conversations) != 1 || c.conversations[0] != key {
		t.Fatalf("the client was not told which conversation it was opening: %v", c.conversations)
	}
}
