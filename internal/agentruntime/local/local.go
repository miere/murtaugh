// Package local builds the in-process agent runtime: the backends, session
// managers, delegation runner and MCP aggregator that run agents on this
// machine.
//
// It is the only implementation of agentruntime.Builder today, and it is
// deliberately a package the Slack gateway binary does not import. Everything
// that can execute a model is reached from here — internal/agentbuild and,
// through it, the three backends — so "which binaries may run an agent" is
// answerable by looking at who imports this package. `cmd/murtaugh` and
// `cmd/murtaugh-runtime` may; `cmd/murtaugh-gateway` may not, and CI checks it
// (#170 Change E).
package local

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agent/native"
	"github.com/miere/murtaugh/internal/agentbuild"
	"github.com/miere/murtaugh/internal/agentdelegate"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/mcpbridge"
	"github.com/miere/murtaugh/internal/nodetoken"
	"github.com/miere/murtaugh/internal/tools"
	"github.com/miere/murtaugh/internal/toolset"
)

// Builder returns the agentruntime.Builder that runs cfg's agents in this
// process. The returned builder is called once per Gateway — including once per
// configuration reload, which is why every backend decision is made inside it
// rather than captured here.
func Builder(cfg config.Config, registry *tools.Registry, logger *slog.Logger) agentruntime.Builder {
	if logger == nil {
		logger = slog.Default()
	}
	return func(hooks agentruntime.Hooks) agentruntime.Runtime {
		return build(cfg, registry, logger, hooks)
	}
}

// Delegator builds the one-shot delegate runner for a host with no MCP
// aggregator — the CLI and MCP frontends, where nothing is listening on a
// socket. Inside the daemon the runtime's own bridged runner is used instead,
// so an agent job fired by cron reaches the same tools a chat agent does.
//
// It returns a genuinely nil interface, not a typed nil, when no agents are
// configured: `jobs.run` nil-checks its delegator to report "agent delegation
// is unavailable", and a typed nil would sail past that check and panic.
func Delegator(cfg config.Config, registry *tools.Registry) agentruntime.Delegator {
	if len(cfg.Agents) == 0 {
		return nil
	}
	return agentdelegate.NewRunner(cfg.Agents, cfg.Defaults, cfg.BaseDir, slog.Default()).
		WithBuildContext(registry, cfg.MCPServers)
}

// SocketPath returns the per-process MCP aggregator socket path. It lives under
// the temp dir (kept short — unix socket paths are length-capped) and carries the
// pid so concurrent gateways do not collide.
func SocketPath() string {
	return filepath.Join(os.TempDir(), "murtaugh", fmt.Sprintf("mcp-agg-%d.sock", os.Getpid()))
}

func build(cfg config.Config, registry *tools.Registry, logger *slog.Logger, hooks agentruntime.Hooks) agentruntime.Runtime {
	rt := agentruntime.Runtime{}
	if len(cfg.Agents) == 0 {
		// No agents: no aggregator to serve, no delegator to hand out. The zero
		// Runtime is exactly what every consumer already handles.
		return rt
	}

	// The aggregator lets ACP and claude_code agents reach Murtaugh's own tools
	// over a private socket. It is built for any configured agent, not just a
	// chat one, because delegated agents (jobs, workflow triggers, unfurls) run
	// even when chat is disabled and need the same surface — a job told to post
	// its result has to be able to.
	bridge := mcpbridge.NewServer(SocketPath(), logger)
	rt.ServeTools = bridge.Start

	if hooks.Chat {
		rt.Sessions = make(map[string]*agent.SessionManager, len(cfg.Agents))
		rt.Clients = make(map[string]agent.Client, len(cfg.Agents))
		rt.ToolProblems = make(map[string][]toolset.Problem)
		for name, profile := range cfg.Agents {
			mgr, client, problems, ok := buildAgent(cfg, registry, logger, hooks, bridge, name, profile)
			if len(problems) > 0 {
				rt.ToolProblems[name] = problems
			}
			if !ok {
				continue
			}
			rt.Sessions[name] = mgr
			rt.Clients[name] = client
		}
	}

	// One shared runner backs every delegate-to-agent surface (jobs, workflow
	// triggers, unfurls). Each delegation spins its own isolated agent process,
	// so this is safe to share. Same build context as a chat agent, bridge
	// included, so a delegated claude_code/ACP agent gets Murtaugh's tools
	// instead of only its own built-ins. No approver: nobody is watching a
	// headless run to answer an approval card, so the agent's own policy is the
	// only gate.
	rt.Delegator = agentdelegate.NewRunner(cfg.Agents, cfg.Defaults, cfg.BaseDir, logger).
		WithBuildContext(registry, cfg.MCPServers).
		WithBridge(bridge)
	return rt
}

// buildAgent resolves one agent's workspace and backend and wraps it in a
// session manager. It reports the tool groups dropped along the way whether or
// not the agent itself built: a degraded agent still answers, and a failed one
// still explains what it lost.
//
// It returns the raw client alongside the manager because a runtime node serves
// the client directly — the gateway on the other end of its link is running the
// session manager for that conversation already.
func buildAgent(
	cfg config.Config,
	registry *tools.Registry,
	logger *slog.Logger,
	hooks agentruntime.Hooks,
	bridge *mcpbridge.Server,
	name string,
	profile config.AgentProfile,
) (*agent.SessionManager, agent.Client, []toolset.Problem, bool) {
	// Resolve the agent's workspace once (workdir → base dir fallback),
	// validated here at the build seam. Any workdir-rooted tool that cannot be
	// rooted is dropped (degraded) rather than failing the agent; the dropped
	// features are surfaced on the startup routing summary.
	resolved, err := agentbuild.Resolve(name, profile, cfg.BaseDir)
	if err != nil {
		logger.Error("agent disabled: could not resolve agent", "agent", name, "kind", profile.ResolvedKind(), "error", err)
		return nil, nil, nil, false
	}
	problems := resolved.Problems()
	for _, p := range problems {
		logger.Warn("agent tool disabled", "agent", name, "tool", p.Group, "reason", p.Reason)
	}
	// Mirror the bundled skills this agent opted to export into its workdir so a
	// filesystem-discovering backend can load them; the default (empty) leaves
	// them in-binary only. Non-fatal: a failure just means no filesystem skills
	// for this agent. Skipped when the agent has no workspace (nothing to export
	// into).
	if agentWorkDir := resolved.Dir(); agentWorkDir != "" {
		if exported, err := config.ReconcileExportedSkills(agentWorkDir, profile.ExportSkillsToFS); err != nil {
			logger.Warn("skill export failed", "agent", name, "error", err)
		} else if len(exported) > 0 {
			logger.Info("exported bundled skills to workdir", "agent", name, "skills", exported, "dir", filepath.Join(agentWorkDir, ".agents", "skills"))
		}
		if _, err := config.ScaffoldWorkspaceDocs(agentWorkDir); err != nil {
			logger.Warn("workspace doc scaffolding failed", "agent", name, "dir", agentWorkDir, "error", err)
		}
	}

	// A missing approver leaves this agent ungated, which is what a headless
	// deployment gets: no thread, nobody to answer a card.
	var approver native.Approver
	if a, ok := hooks.Approvers[name]; ok {
		approver = a
	}
	client, err := agentbuild.Client(resolved, agentbuild.Deps{
		Registry:               registry,
		MCPServers:             cfg.MCPServers,
		WorkspaceDir:           cfg.BaseDir,
		Logger:                 logger.With("agent", name),
		Approver:               approver,
		Bridge:                 bridge,
		LongRunningToolTimeout: cfg.Defaults.EffectiveLongRunningToolTimeout(),
		BackgroundSink:         hooks.BackgroundEvents,
		// Blind a confined agent to this host's node credential. Passed whether
		// or not the file exists: it names the path the enrolment will use, and a
		// rule for a path that is not there yet costs nothing, whereas a rule
		// added only once the file appears would leave a window with the file
		// present and the deny absent.
		NodeTokenPath: nodetoken.PathFor(cfg.BaseDir),
	})
	if err != nil {
		logger.Error("agent disabled: could not build client", "agent", name, "kind", profile.ResolvedKind(), "error", err)
		return nil, nil, problems, false
	}
	var interruptible *bool
	if profile.ACP != nil {
		interruptible = profile.ACP.Interruptible
	}
	mgr := agent.NewSessionManager(
		client,
		cfg.Defaults.EffectiveSessionIdleTimeout(),
		cfg.Defaults.EffectiveMaxSessions(),
	).WithLogger(logger.With("agent", name)).
		WithBusyTimeout(cfg.Defaults.EffectiveSessionBusyTimeout()).
		WithCancelOverride(interruptible).
		WithDescriptor(string(profile.ResolvedKind()), profile.ResolvedApproval())
	return mgr, client, problems, true
}
