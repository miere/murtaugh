package nodeserve

import (
	"context"
	"errors"

	"github.com/miere/murtaugh/internal/agent"
)

// ErrNodeUnconfigured is what every turn asked of a node with no agent profile
// answers with.
//
// It is a sentinel because it is the one failure on this path that has an action
// attached: the owner has not finished setting the node up, and the gateway has
// already offered them the form. A generic "agent failed" would send whoever
// read it looking for a broken model instead.
var ErrNodeUnconfigured = errors.New("this runtime node has no agent profile configured yet")

// UnconfiguredClient is the agent a never-configured node serves.
//
// # Why a node with nothing to run attaches at all
//
// #170 Change I makes a node connecting with zero profiles the trigger for
// onboarding its owner through Slack. That trigger is only reachable if such a
// node can CONNECT: the gateway learns who owns a node from the credential the
// connection presented, and it learns the node has nothing from the empty
// advertisement that arrives inside the handshake. A node that refused to start
// without a profile — which is what it did before this item — could never
// deliver either fact, so the onboarding trigger was unreachable by
// construction.
//
// # Why it initializes successfully and fails every turn
//
// The gateway does not publish a node whose Initialize fails
// (internal/nodehost/host.go), and an unpublished node is invisible: no
// registry entry, no owner, no trigger. So Initialize succeeds — it is
// truthfully reporting that the link is up — and the refusal lands on the one
// call that would have needed a model.
//
// It should never be reached in practice, because a node advertising no
// profiles is a node delegation will not choose: the fleet's matching step has
// nothing to match. The refusal is what makes that "should" observable rather
// than a hang.
type UnconfiguredClient struct{}

// Initialize succeeds: the link is genuinely up, and it is the empty
// advertisement rather than a failed handshake that says the node is not
// configured.
func (UnconfiguredClient) Initialize(context.Context) error { return nil }

// NewSession refuses. There is no backend to open a session against.
func (UnconfiguredClient) NewSession(context.Context, agent.SessionMetadata) (agent.Session, error) {
	return agent.Session{}, ErrNodeUnconfigured
}

// Prompt refuses.
func (UnconfiguredClient) Prompt(context.Context, string, agent.PromptRequest) (<-chan agent.Event, error) {
	return nil, ErrNodeUnconfigured
}

// Cancel succeeds: there is nothing running, so the caller's request that
// nothing be running is already true. Refusing would turn a no-op into an error
// on a teardown path.
func (UnconfiguredClient) Cancel(context.Context, string) error { return nil }

// Close succeeds. There is nothing to release.
func (UnconfiguredClient) Close() error { return nil }
