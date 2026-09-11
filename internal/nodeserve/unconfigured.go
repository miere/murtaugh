package nodeserve

import (
	"context"
	"errors"

	"github.com/miere/murtaugh/internal/agent"
)

// A sentinel because it is the one failure here with an action: the owner has not finished setup,
// and a generic error would send them looking for a broken model.
var ErrNodeUnconfigured = errors.New("this runtime node has no agent profile configured yet")

// Lets a node with no agent still connect: the gateway needs its credential and empty advertisement
// to learn who owns it and offer onboarding.
type UnconfiguredClient struct{}

// Succeeds because the gateway never publishes a node whose Initialize fails, and an unpublished
// node cannot trigger onboarding.
func (UnconfiguredClient) Initialize(context.Context) error { return nil }

func (UnconfiguredClient) NewSession(context.Context, agent.SessionMetadata) (agent.Session, error) {
	return agent.Session{}, ErrNodeUnconfigured
}

func (UnconfiguredClient) Prompt(context.Context, string, agent.PromptRequest) (<-chan agent.Event, error) {
	return nil, ErrNodeUnconfigured
}

// Succeeds: nothing is running, and refusing would turn a no-op into an error on a teardown path.
func (UnconfiguredClient) Cancel(context.Context, string) error { return nil }

func (UnconfiguredClient) Close() error { return nil }
