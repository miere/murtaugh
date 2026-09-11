// Package agentruntime is the seam between the Slack gateway and the machinery
// that actually builds and runs agents.
//
// It exists because of #170 Concern 1: the gateway must be *incapable* of
// running an agent, not merely disinclined to. That is a reachability property,
// so it cannot be expressed with an interface the gateway declares and the
// builder happens to satisfy — the builder would have to name the gateway's
// types, and the gateway's own package would end up importing the thing it must
// not reach. Both ends name types from here instead, and here imports nothing
// that can run a model.
//
// Nothing in this package builds anything. internal/agentruntime/local is the
// in-process implementation (the one the CLI and today's daemon use); a runtime
// backed by remote nodes will be another, and the gateway will not be able to
// tell them apart.
package agentruntime

import (
	"context"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/toolset"
)

// Approver gates a side-effecting tool call behind human approval. The gateway
// supplies one per agent (it is the only side that can ask a human), and the
// runtime hands it to whichever backend consults it.
//
// It is declared here, rather than taken from the backend that consumes it, for
// the same reason as everything else in this package: the gateway must be able
// to build an approver without importing an agent backend.
type Approver interface {
	// Approve asks the user to confirm a side-effecting tool call. summary is a
	// short human description (e.g. the shell command). It returns whether to run
	// the tool and, when not, a note to feed back to the model as the tool result.
	Approve(ctx context.Context, toolName, summary string) (allowed bool, note string)
}

// Delegator is the shared one-shot agent runner behind every delegate-to-agent
// surface: workflow triggers and unfurls take JSON back, scheduled jobs and the
// `jobs.run` tool do not wait at all.
//
// A caller that needs only one of the two verbs keeps declaring its own
// narrower interface (workflow.AgentDelegator, gateway.UnfurlDelegator,
// run.AgentDelegator) — this is the union the runtime hands over, not a
// replacement for those.
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

// Hooks are what the gateway contributes to the runtime: the collaborators only
// a Slack-facing process can supply. They are passed to a Builder rather than
// set afterwards because the runtime hands them to each backend at construction.
type Hooks struct {
	// Chat reports whether the host will serve chat turns. When false the runtime
	// builds no session managers, because nothing would ever prompt them — while
	// still building the delegation surfaces, which run whether or not chat is
	// enabled: a scheduled job, a workflow trigger and an unfurl all delegate
	// with no thread in sight.
	//
	// Building one spawns nothing, so this is not a resource argument: every
	// backend constructor only fills a struct (the ACP client, claudecode, the
	// aggregator's MCP servers are all lazy), and a process appears at
	// Initialize/NewSession. The cost of building them anyway would be a map
	// entry — and a set of managers whose existence says the host serves chat
	// when it does not.
	Chat bool
	// Approvers is the per-agent approval gate, keyed by agent name. A missing
	// entry leaves that agent ungated, which is what a headless deployment (chat
	// disabled) gets — nobody is in a thread to answer a card.
	Approvers map[string]Approver
	// BackgroundEvents receives events a session emits with no active turn — a
	// claude_code background subagent completing after its turn ended. nil drops
	// them.
	BackgroundEvents func(sessionID string, ev agent.Event)
}

// Runtime is one built set of agents plus the shared surfaces they need. It is a
// struct of already-resolved values rather than an interface because every field
// is a fact decided at construction; there is no behaviour left to dispatch on.
//
// The zero Runtime is meaningful and is what a gateway that cannot run agents
// gets: no sessions, no delegator, nothing to serve. Every consumer already
// handles that state, because it is also what a deployment with no agents
// configured has always produced.
type Runtime struct {
	// Sessions is one session manager per agent that built successfully, keyed by
	// agent name. An agent that failed to build is absent, and the startup
	// routing summary reports the gap.
	Sessions map[string]*agent.SessionManager
	// Clients is the raw agent.Client under each session manager, keyed the same
	// way. A gateway never touches it — it holds conversations and therefore
	// wants the manager. A runtime node does: it serves a gateway that runs its
	// OWN session manager over the link, and putting a second one on the node
	// would mint a second set of session ids for the same conversation.
	Clients map[string]agent.Client
	// ToolProblems records, per agent, the tool groups dropped at build time
	// because the agent had no resolvable workspace. The agent still answers;
	// the startup summary reports each dropped feature.
	ToolProblems map[string][]toolset.Problem
	// Delegator backs every delegate-to-agent surface. nil when no agent is
	// configured, which the surfaces report as "agent delegation is unavailable"
	// rather than doing nothing.
	Delegator Delegator
	// ServeTools runs whatever the agents' Murtaugh-tool surface needs — today
	// the MCP aggregator socket an acp/claude_code agent reaches through
	// `murtaugh mcp-bridge` — until ctx ends, then tears it down. It is called on
	// every leader promotion and must therefore be restartable. nil means there
	// is nothing to serve.
	ServeTools func(ctx context.Context) error
}

// Builder constructs a Runtime from the hooks its host supplies. The
// configuration, tool registry and logger are the builder's own business: it is
// made by the composition root, which has them.
//
// A nil Builder is how a host says "this process may not run agents". Callers
// treat it as a builder returning the zero Runtime.
type Builder func(Hooks) Runtime
