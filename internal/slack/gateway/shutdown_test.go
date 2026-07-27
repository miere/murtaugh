package gateway

import (
	"context"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
)

// closerSessions is a ChatSessionManager that records Close calls.
type closerSessions struct{ closed *int }

func (closerSessions) Prompt(context.Context, agent.ConversationKey, agent.SessionMetadata, agent.PromptRequest) (<-chan agent.Event, error) {
	return nil, nil
}
func (closerSessions) Lookup(agent.ConversationKey) (string, bool) { return "", false }
func (closerSessions) Cancel(context.Context, string) error        { return nil }
func (c closerSessions) Close() error                              { *c.closed++; return nil }

// nonCloserSessions is a ChatSessionManager that is NOT an io.Closer (must be
// skipped without panicking).
type nonCloserSessions struct{}

func (nonCloserSessions) Prompt(context.Context, agent.ConversationKey, agent.SessionMetadata, agent.PromptRequest) (<-chan agent.Event, error) {
	return nil, nil
}
func (nonCloserSessions) Lookup(agent.ConversationKey) (string, bool) { return "", false }
func (nonCloserSessions) Cancel(context.Context, string) error        { return nil }

func TestCloseChatSessions_ClosesEveryManager(t *testing.T) {
	n := 0
	g := &Gateway{
		logger: discardLogger(),
		chatSessions: map[string]ChatSessionManager{
			"coder":   closerSessions{closed: &n},
			"default": closerSessions{closed: &n},
			"legacy":  nonCloserSessions{}, // not a Closer — skipped
		},
	}
	g.closeChatSessions()
	if n != 2 {
		t.Fatalf("expected Close on both closable managers, got %d", n)
	}
}
