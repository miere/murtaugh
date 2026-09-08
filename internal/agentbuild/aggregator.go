package agentbuild

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agent/native"
	"github.com/miere/murtaugh/internal/frontends/mcp"
	"github.com/miere/murtaugh/internal/mcpbridge"
	"github.com/miere/murtaugh/internal/mcpclient"
	"github.com/miere/murtaugh/internal/tools"
	"github.com/miere/murtaugh/internal/toolset"
)

// acpAggregator is the concrete agent.Aggregator for an ACP agent. It serves the
// agent's resolved toolset over the gateway's shared bridge socket, gated by the
// same human approver the native loop uses. The toolset is the agent's built-ins
// (its tools: allowlist, minus the native-only synthesized groups and the
// host-config-mutating tools) plus the proxied tools of every authoritative
// external MCP server — so the ACP agent sees the same Murtaugh surface a native
// agent would, with third-party credentials staying inside the gateway.
type acpAggregator struct {
	server *mcpbridge.Server
	binary string
	// registry and resolved are held rather than resolved into a []tools.Tool at
	// construction. See resolvedToolset: on a runtime node the registry is EMPTY
	// when this is built and is filled at the gateway handshake, so a snapshot
	// taken here is a snapshot of nothing.
	registry *tools.Registry
	resolved ResolvedAgent
	approver mcp.Approver
	mcpCfgs  []mcpclient.ServerConfig
	logger   *slog.Logger
	// agentEnv is the profile's own environment, carried onto every tool-call
	// context so a bridged tool that spawns a process on this agent's behalf
	// spawns it with the AGENT's environment rather than the daemon's. See
	// agent.WithTurnEnv for why that distinction has teeth.
	agentEnv []string

	// The built-ins are resolved and the external MCP servers opened once, on
	// the first session. The MCP half is lazy because it is network I/O kept out
	// of gateway startup; the built-in half is lazy because of WHEN the registry
	// is filled.
	once       sync.Once
	mgr        *mcpclient.Manager
	toolset    []tools.Tool
	resolveErr error
}

// newACPAggregator records what the agent's toolset will be resolved from and
// the authoritative external MCP servers to proxy. resolved carries the agent's
// effective (pruned) allowlist and its workspace Root; approver (may be nil)
// gates side-effecting calls; mcpCfgs is the global, authoritative MCP server set
// (native.MCPServerConfigs).
//
// It deliberately does NOT resolve the toolset here. See resolvedToolset.
func newACPAggregator(server *mcpbridge.Server, registry *tools.Registry, resolved ResolvedAgent, approver mcp.Approver, mcpCfgs []mcpclient.ServerConfig, logger *slog.Logger) (*acpAggregator, error) {
	binary, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve murtaugh binary for bridge: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &acpAggregator{
		server:   server,
		binary:   binary,
		registry: registry,
		resolved: resolved,
		approver: approver,
		mcpCfgs:  mcpCfgs,
		logger:   logger,
		agentEnv: resolved.Profile.EnvOverrides(),
	}, nil
}

// resolvedToolset resolves the agent's built-ins and opens the external MCP
// servers once (lazily), and returns the full served toolset: built-ins followed
// by the proxied MCP tools.
//
// # Why the registry is read HERE and not at construction
//
// On a runtime node the registry this aggregator is built from is
// nodeserve.ToolProxy's, and it is EMPTY at construction: the node builds its
// agent before it has a gateway connection, and the proxy is filled at the
// handshake (nodeserve.serveInitialize). An aggregator that snapshotted the
// registry when it was built therefore served zero tools for the whole life of
// the process — an `acp` or `claude_code` agent on a node reached nothing, while
// `native` worked because it resolves inside its own Initialize, which runs
// after the handshake. That is the regression #194 exists to prevent, in the two
// backends it was written for.
//
// Reading it on the first session is the earliest point that is late enough for
// every caller: a node has completed its handshake by then (the gateway sends
// initialize before session.new), and in the gateway process the registry is
// built once in app.New, before any agent, so nothing changes there but the
// moment the work happens.
//
// A resolve failure is remembered and returned to every caller rather than
// swallowed here. What the two callers then DO with it is unchanged and is worth
// stating rather than implying: both `acp` and `claude_code` downgrade it to a
// warning and run the session tool-less (acp/session.go, claudecode.go). So this
// is not a session that refuses — the error is merely reported at the point that
// can name the session instead of at construction. The branch is unreachable
// today in any case: toolset.Resolve returns no error, degrading via []Problem.
func (a *acpAggregator) resolvedToolset() ([]tools.Tool, error) {
	a.once.Do(func() {
		builtins, err := resolveBuiltins(a.registry, a.resolved)
		if err != nil {
			a.resolveErr = err
			return
		}
		a.mgr = mcpclient.Open(context.Background(), a.mcpCfgs, a.logger)
		mcpTools := a.mgr.Tools()
		merged := make([]tools.Tool, 0, len(builtins)+len(mcpTools))
		merged = append(merged, builtins...)
		merged = append(merged, mcpTools...)
		a.toolset = merged
		a.logger.Info("acp aggregator toolset resolved", "builtins", len(builtins), "mcp_tools", len(mcpTools), "mcp_servers", len(a.mcpCfgs))
	})
	return a.toolset, a.resolveErr
}

// Close tears down the proxied MCP connections. Safe to call when none were ever
// opened (no session used the aggregator).
func (a *acpAggregator) Close() error {
	if a.mgr != nil {
		return a.mgr.Close()
	}
	return nil
}

// RegisterSession registers this session's toolset under a fresh token and
// returns the stdio bridge server to advertise. The session's Slack location is
// injected into every tool-call context so the approver posts in the right
// thread.
func (a *acpAggregator) RegisterSession(meta agent.SessionMetadata) (agent.MCPServerSpec, func(), error) {
	served, err := a.resolvedToolset()
	if err != nil {
		return agent.MCPServerSpec{}, nil, fmt.Errorf("resolve this agent's toolset: %w", err)
	}
	decorate := turnDecorator(meta, a.agentEnv)
	token, err := a.server.Register(mcpbridge.Session{
		Tools:       served,
		Approver:    a.approver,
		WithContext: decorate,
	})
	if err != nil {
		return agent.MCPServerSpec{}, nil, err
	}
	spec := agent.MCPServerSpec{
		Name:    "murtaugh",
		Command: a.binary,
		Args:    []string{mcpbridge.Subcommand},
		Env: map[string]string{
			mcpbridge.EnvSocket: a.server.SocketPath(),
			mcpbridge.EnvToken:  token,
		},
	}
	return spec, func() { a.server.Unregister(token) }, nil
}

// turnDecorator returns a context decorator that carries what a bridged tool
// needs to know about its caller: the session's Slack location (so an
// interactive tool posts in the right thread) and the agent's own environment
// (so a tool that spawns a process spawns it as the agent, not as the daemon).
//
// It returns nil only when there is NEITHER — a non-chat session for an agent
// with no environment stays undecorated, matching GateApprover's headless
// behaviour. A headless session for an agent that DOES have an environment still
// gets it: a delegated job authenticating gcloud has the same split-brain problem
// a chat turn does, and no thread to report it in.
func turnDecorator(meta agent.SessionMetadata, agentEnv []string) func(context.Context) context.Context {
	hasLocation := strings.TrimSpace(meta.ChannelID) != ""
	if !hasLocation && len(agentEnv) == 0 {
		return nil
	}
	loc := agent.TurnLocation{ChannelID: meta.ChannelID, ThreadTS: meta.ThreadTS}
	return func(ctx context.Context) context.Context {
		if hasLocation {
			ctx = agent.WithTurnLocation(ctx, loc)
		}
		return agent.WithTurnEnv(ctx, agentEnv)
	}
}

// resolveBuiltins resolves the registry tools an ACP agent's allowlist selects,
// excluding the native-only synthesized groups (files/terminal/skills — the
// agent has its own) and the bridge-unsafe tools (setup.*, which mutate
// Murtaugh's own configuration and must never be handed to an external agent).
// The native-only set is read from toolset.IsNativeOnlyForACP (the single source
// of truth), so `attach` is NOT stripped — it is served to ACP agents, rooted at
// the resolved workspace. Any workdir-rooted group whose root could not be built
// was already pruned by the seam, so it does not reach here.
func resolveBuiltins(registry *tools.Registry, resolved ResolvedAgent) ([]tools.Tool, error) {
	allow := resolved.Tools()
	filtered := make([]string, 0, len(allow))
	for _, a := range allow {
		if toolset.IsNativeOnlyForACP(strings.TrimSpace(a)) {
			continue
		}
		filtered = append(filtered, a)
	}
	ts, _, err := toolset.Resolve(filtered, nil, toolset.Deps{
		Registry:   registry,
		Root:       resolved.Root(),
		AgentName:  resolved.Name(),
		RootReason: resolved.RootReason(),
	})
	if err != nil {
		return nil, err
	}
	out := ts[:0]
	for _, t := range ts {
		if bridgeUnsafe(t.Name()) {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// bridgeUnsafe reports whether a tool must never be exposed to an external ACP
// agent regardless of the allowlist. The setup.* family writes Murtaugh's own
// config files (config.yaml, .env, agents.yaml); handing those to an outside
// agent would let it reconfigure the host.
func bridgeUnsafe(name string) bool {
	return name == "setup" || strings.HasPrefix(name, "setup.")
}

// mcpApprover adapts a native.Approver into the mcp.Approver the aggregator
// expects. Both have the same method, so this just reuses the gateway's existing
// GateApprover (the same Slack approval gate the native loop uses). A nil inner
// approver means ungated.
type mcpApprover struct{ inner native.Approver }

func (m mcpApprover) Approve(ctx context.Context, toolName, summary string) (bool, string) {
	if m.inner == nil {
		return true, ""
	}
	return m.inner.Approve(ctx, toolName, summary)
}
