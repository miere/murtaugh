// Package agentbuild constructs the agent.Client backend for an agent profile,
// branching on its kind: a kind:native profile yields the in-process LLM loop
// (internal/agent/native); a kind:acp profile yields the external-process
// ProcessClient. It is the single place the two backends are selected, shared by
// the Slack gateway (chat sessions) and the agentdelegate runner (jobs,
// workflows, unfurls) so both gain native support from one seam.
package agentbuild

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agent/acp"
	"github.com/miere/murtaugh/internal/agent/claudecode"
	"github.com/miere/murtaugh/internal/agent/native"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/frontends/mcp"
	"github.com/miere/murtaugh/internal/mcpbridge"
	"github.com/miere/murtaugh/internal/tools"
)

// Deps carries the shared context needed to build either backend. Registry and
// MCPServers are only consulted for native agents; WorkspaceDir is the
// workspace/config root for persona (SOUL.md), the system-prompt file, and the
// bespoke-skills dir. The agent workdir is NOT here — it is resolved once into
// the ResolvedAgent passed to Client, so it cannot be re-derived from a raw
// fallback at this seam.
type Deps struct {
	Registry     *tools.Registry
	MCPServers   map[string]config.MCPServerConfig
	WorkspaceDir string
	Logger       *slog.Logger
	// Approver gates a native agent's side-effecting tool calls behind human
	// approval. nil disables gating — set only on the interactive chat path,
	// never for headless/delegated agents. Ignored for ACP agents.
	Approver native.Approver
	// Bridge is the gateway's shared MCP aggregator server. When set, an ACP
	// agent is given a per-agent aggregator over it so it can reach Murtaugh's
	// own tools through `murtaugh mcp-bridge`. Both daemon paths set it — chat
	// and delegation (jobs, workflow triggers, unfurls) — so a claude_code or
	// ACP agent has the same tools either way. nil (the CLI path, where no
	// aggregator is listening) leaves it with no Murtaugh tools. Ignored for
	// native agents, which hold their toolset in-process.
	Bridge *mcpbridge.Server
	// LongRunningToolTimeout is the per-tool ceiling passed to an ACP or
	// claude_code agent (see SessionDefaults.LongRunningToolTimeout) — both honour
	// it identically, so the operator's setting does not depend on which backend
	// answers. Zero leaves the ProcessClient
	// default. Ignored for native agents.
	LongRunningToolTimeout time.Duration
	// BackgroundSink receives events a claude_code session emits with no active
	// turn — a background subagent completing after its turn ended, then the model
	// auto-continuing. The gateway renders them into the originating Slack thread.
	// nil (CLI/delegate paths, other backends) drops them. Ignored by acp/native.
	BackgroundSink func(sessionID string, ev agent.Event)
}

// Client builds the backend for a resolved agent. It does no network/process I/O
// — both backends defer that to Initialize. The agent's workdir is taken from
// resolved (already validated); deps carries only the workspace/config root and
// the shared wiring.
func Client(resolved ResolvedAgent, deps Deps) (agent.Client, error) {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	profile := resolved.Profile

	// Resolve the sandbox once, here, where the profile and the runtime facts it
	// cannot know (the resolved workdir, the bridge socket path) first co-exist —
	// the same validated-core rule the workspace root follows. A failure is fatal
	// rather than a degrade: dropping a security boundary silently is worse than an
	// agent that refuses to start.
	box, err := resolveSandbox(profile, resolved, deps.Bridge)
	if err != nil {
		return nil, err
	}

	switch resolved.Kind {
	case config.AgentKindNative:
		return native.Build(profile, native.BuildDeps{
			Registry:     deps.Registry,
			MCPServers:   deps.MCPServers,
			WorkspaceDir: deps.WorkspaceDir,
			Root:         resolved.Root(),
			Tools:        resolved.Tools(),
			Logger:       logger,
			Approver:     deps.Approver,
		})
	case config.AgentKindACP:
		var aggregator agent.Aggregator
		if deps.Bridge != nil {
			var approver mcp.Approver
			if deps.Approver != nil {
				approver = mcpApprover{inner: deps.Approver}
			}
			aggr, err := newACPAggregator(deps.Bridge, deps.Registry, resolved, approver, native.MCPServerConfigs(deps.MCPServers), logger)
			if err != nil {
				return nil, fmt.Errorf("agentbuild: build ACP aggregator: %w", err)
			}
			aggregator = aggr
		}
		return acp.NewProcessClient(acp.ProcessOptions{
			Command:          profile.ACP.Command,
			Args:             profile.ACP.Args,
			WorkDir:          resolved.Dir(),
			Env:              profile.EnvOverrides(),
			Logger:           logger,
			PermissionPolicy: profile.ResolvedACPPermission(),
			Aggregator:       aggregator,
			// Share Murtaugh's persona with the ACP agent (it has no system role of
			// our making); read from the config/workspace dir where SOUL.md lives.
			Persona:     native.ReadSoul(deps.WorkspaceDir),
			ToolCeiling: deps.LongRunningToolTimeout,
			Sandbox:     box,
		}), nil
	case config.AgentKindClaudeCode:
		// Direct Claude Code stream-json backend (spec 019). Tool permissions route
		// to a human in Slack via EventPermission (same approval.requests policy as
		// ACP).
		//
		// Wire Murtaugh's own tools the same way ACP does — a per-agent aggregator
		// served over the shared stdio bridge — so a claude_code agent reaches the
		// same tool surface (slack.*, jobs, …) instead of only the claude CLI's own
		// MCP servers. The claudecode backend advertises this to the `claude`
		// process via --mcp-config.
		var aggregator agent.Aggregator
		if deps.Bridge != nil {
			var approver mcp.Approver
			if deps.Approver != nil {
				approver = mcpApprover{inner: deps.Approver}
			}
			aggr, err := newACPAggregator(deps.Bridge, deps.Registry, resolved, approver, native.MCPServerConfigs(deps.MCPServers), logger)
			if err != nil {
				return nil, fmt.Errorf("agentbuild: build claude_code aggregator: %w", err)
			}
			aggregator = aggr
		}
		return claudecode.New(claudecode.Options{
			Command:          profile.ClaudeCode.Command,
			Args:             profile.ClaudeCode.Args,
			Model:            profile.ClaudeCode.Model,
			Env:              profile.EnvOverrides(),
			WorkDir:          resolved.Dir(),
			Logger:           logger,
			PermissionPolicy: profile.ResolvedACPPermission(),
			OnBackground:     deps.BackgroundSink,
			Aggregator:       aggregator,
			Sandbox:          box,
			ToolCeiling:      deps.LongRunningToolTimeout,
		}), nil
	default:
		return nil, fmt.Errorf("agentbuild: unknown agent kind %q", resolved.Kind)
	}
}

// ErrorClient returns an agent.Client that fails every operation with err. It
// lets callers whose factory signature cannot return an error (e.g.
// agentdelegate.ClientFactory) surface a build failure at Initialize time
// instead of panicking on a nil client.
func ErrorClient(err error) agent.Client { return errClient{err: err} }

type errClient struct{ err error }

func (c errClient) Initialize(context.Context) error { return c.err }
func (c errClient) NewSession(context.Context, agent.SessionMetadata) (agent.Session, error) {
	return agent.Session{}, c.err
}
func (c errClient) Prompt(context.Context, string, agent.PromptRequest) (<-chan agent.Event, error) {
	return nil, c.err
}
func (c errClient) Cancel(context.Context, string) error { return c.err }
func (c errClient) Close() error                         { return nil }
