// Package local runs agents on this machine. cmd/murtaugh-gateway must never
// import it: CI checks that the gateway binary cannot reach an agent backend.
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

// Builder's result is called again on every configuration reload, so every
// backend decision is made inside it rather than captured here.
func Builder(cfg config.Config, registry *tools.Registry, logger *slog.Logger) agentruntime.Builder {
	if logger == nil {
		logger = slog.Default()
	}
	return func(hooks agentruntime.Hooks) agentruntime.Runtime {
		return build(cfg, registry, logger, hooks)
	}
}

// Delegator returns an untyped nil when no agents are configured: jobs.run
// nil-checks it, and a typed nil would slip past that check.
func Delegator(cfg config.Config, registry *tools.Registry) agentruntime.Delegator {
	if len(cfg.Agents) == 0 {
		return nil
	}
	return agentdelegate.NewRunner(cfg.Agents, cfg.Defaults, cfg.BaseDir, slog.Default()).
		WithBuildContext(registry, cfg.MCPServers)
}

// SocketPath stays short because unix socket paths are length-capped, and holds
// the pid so two gateways on one machine never share a socket.
func SocketPath() string {
	return filepath.Join(os.TempDir(), "murtaugh", fmt.Sprintf("mcp-agg-%d.sock", os.Getpid()))
}

func build(cfg config.Config, registry *tools.Registry, logger *slog.Logger, hooks agentruntime.Hooks) agentruntime.Runtime {
	rt := agentruntime.Runtime{InProcess: true}
	if len(cfg.Agents) == 0 {
		return rt
	}

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

	rt.Delegator = agentdelegate.NewRunner(cfg.Agents, cfg.Defaults, cfg.BaseDir, logger).
		WithBuildContext(registry, cfg.MCPServers).
		WithBridge(bridge)
	return rt
}

func buildAgent(
	cfg config.Config,
	registry *tools.Registry,
	logger *slog.Logger,
	hooks agentruntime.Hooks,
	bridge *mcpbridge.Server,
	name string,
	profile config.AgentProfile,
) (*agent.SessionManager, agent.Client, []toolset.Problem, bool) {
	resolved, err := agentbuild.Resolve(name, profile, cfg.BaseDir)
	if err != nil {
		logger.Error("agent disabled: could not resolve agent", "agent", name, "kind", profile.ResolvedKind(), "error", err)
		return nil, nil, nil, false
	}
	problems := resolved.Problems()
	for _, p := range problems {
		logger.Warn("agent tool disabled", "agent", name, "tool", p.Group, "reason", p.Reason)
	}
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
		NodeTokenPath:          nodetoken.PathFor(cfg.BaseDir),
	})
	if err != nil {
		logger.Error("agent disabled: could not build client", "agent", name, "kind", profile.ResolvedKind(), "error", err)
		return nil, nil, problems, false
	}
	mgr := agent.NewSessionManager(
		client,
		cfg.Defaults.EffectiveSessionIdleTimeout(),
		cfg.Defaults.EffectiveMaxSessions(),
	).WithLogger(logger.With("agent", name)).
		WithBusyTimeout(cfg.Defaults.EffectiveSessionBusyTimeout()).
		WithCancelOverride(profile.CancelOverride()).
		WithDescriptor(string(profile.ResolvedKind()), profile.ResolvedApproval())
	return mgr, client, problems, true
}
