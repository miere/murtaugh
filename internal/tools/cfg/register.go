package cfg

import (
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/tools"
)

// GatewayTools returns the `cfg …` surface the gateway binary carries: what the
// gateway admin owns (access, jobs, workflow and unfurl rules, election), plus
// `cfg node split`, which reads this store to seed a node's own.
func GatewayTools(store config.Store, configPath string, installer InstallerDeps) []tools.Tool {
	p := NewProvider(store, config.RoleGateway)
	var out []tools.Tool
	out = append(out, JobTools(p)...)
	out = append(out, RuleTools(p)...)
	out = append(out, GatewaySingletonTools(p)...)
	out = append(out, AdminTools(p)...)
	out = append(out, DBTools(p, configPath)...)
	out = append(out, NodeSplitTools(p, configPath)...)
	out = append(out, InstallerTools(installer)...)
	return out
}

// NodeTools returns the `cfg …` surface the runtime binary carries: the agent
// profiles and MCP servers it runs, and where it dials.
func NodeTools(store config.Store, configPath string, installer InstallerDeps) []tools.Tool {
	p := NewProvider(store, config.RoleNode)
	var out []tools.Tool
	out = append(out, AgentTools(p)...)
	out = append(out, McpTools(p)...)
	out = append(out, NodeSingletonTools(p)...)
	out = append(out, AdminTools(p)...)
	out = append(out, DBTools(p, configPath)...)
	out = append(out, InstallerTools(installer)...)
	return out
}
