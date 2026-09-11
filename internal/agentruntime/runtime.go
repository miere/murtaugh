// Package agentruntime holds the types the gateway and agent builders share. It
// must never import anything that can run a model, or the gateway could too.
package agentruntime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/toolset"
)

// Approver is declared here, not in the backend that uses it, so the gateway can
// build one without importing an agent backend.
type Approver interface {
	Approve(ctx context.Context, toolName, summary string) (allowed bool, note string)
}

// Delegator is the union the runtime hands over; callers that need one verb keep
// their own narrower interface instead of depending on this one.
type Delegator interface {
	RunForJSON(ctx context.Context, agent, prompt string) ([]byte, error)
	RunAndForget(ctx context.Context, agent, prompt string) error
}

// Reply exists because whether the gateway may repeat a delegation's answer
// depends on whose machine produced it.
type Reply struct {
	Text string `json:"text"`
	// A zero Reply must never pass for the gateway's own run, so only an
	// in-process runner sets this.
	InProcess bool   `json:"in_process,omitempty"`
	NodeID    string `json:"node_id,omitempty"`
	NodeOwner string `json:"node_owner,omitempty"`
}

// Hooks go to a Builder rather than being set afterwards because the runtime
// hands them to each backend when it constructs it.
type Hooks struct {
	// Chat false still builds the delegation surfaces: jobs, workflow triggers and
	// unfurls delegate with no chat thread at all.
	Chat bool
	// A missing entry leaves that agent ungated, which suits a headless deployment:
	// nobody is in a thread to answer the approval card.
	Approvers        map[string]Approver
	BackgroundEvents func(sessionID string, ev agent.Event)

	SignIn func(ctx context.Context, prompt *agent.SignInPrompt, settled <-chan agent.SignInSettled, shown func(error))

	CredentialHealth func(CredentialHealth)
}

// CredentialHealth is one node's word on one of its credentials; the node and
// owner come from the connection, so a node cannot speak for anyone else.
type CredentialHealth struct {
	NodeID     string
	Owner      string
	Credential string
	Degraded   bool
	Reason     string
	Since      time.Time
	ExpiresAt  time.Time
	ReportedAt time.Time
}

// The zero Runtime is what a gateway that cannot run agents gets; every consumer
// already handles it because "no agents configured" produces it too.
type Runtime struct {
	Sessions map[string]*agent.SessionManager
	// Clients is for runtime nodes: their gateway runs the session manager over the
	// link, and a second manager on the node would mint duplicate session ids.
	Clients      map[string]agent.Client
	ToolProblems map[string][]toolset.Problem
	Delegator    Delegator
	// ServeTools is called again on every leader promotion, so it must be safe to
	// run more than once.
	ServeTools func(ctx context.Context) error

	InProcess         bool
	CredentialReports func() []CredentialHealth

	PinnedNode      func(ctx context.Context, conversation agent.ConversationKey) (NodeRef, error)
	ConnectedNodes  func() []NodeRef
	RenewCredential func(ctx context.Context, nodeID string) (RenewalStatus, error)
}

// ErrNotPinned is refused rather than answered with some other node, because
// signing in a machine the conversation does not run on fixes nothing.
var ErrNotPinned = errors.New("this conversation is not running on any runtime node yet")

// Declared here rather than in nodehost so the gateway can tell a missing machine
// from a fault without linking the node host.
var ErrNoNode = errors.New("no runtime node is connected")

// Separate from ErrNoNode: nothing connected is the admin's problem, while
// none of the user's own nodes connected is the user's.
var ErrNoFleet = errors.New("no runtime node of yours is connected, and you hold no grant on another")

// NodeOfflineError keeps the pinned node with the refusal, so the user is told
// which machine went away rather than only that none is left.
type NodeOfflineError struct {
	Node NodeRef
	Err  error
}

func (e *NodeOfflineError) Error() string {
	return fmt.Sprintf("node %s, which this conversation runs on, is not connected: %v", e.Node.NodeID, e.Err)
}

func (e *NodeOfflineError) Unwrap() error { return e.Err }

// NodeRef carries the owner with the node, because only they or the admin may
// have a sign-in run on it.
type NodeRef struct {
	NodeID string
	Owner  string
}

// RenewalStatus is reported back as it is, so nobody is told a sign-in is on its
// way when one was already open or there was nothing to sign in.
type RenewalStatus string

const (
	RenewalStarted        RenewalStatus = "started"
	RenewalAlreadyRunning RenewalStatus = "already_running"
	RenewalNothingToRenew RenewalStatus = "none"
)

// A nil Builder means "this process may not run agents"; callers treat it as a
// builder that returns the zero Runtime.
type Builder func(Hooks) Runtime
