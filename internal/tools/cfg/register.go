package cfg

import (
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/tools"
)

// All returns every `cfg …` tool bound to the given store. The composition root
// registers them all so the admin surface is exposed identically over the CLI
// and MCP. Pass a store obtained from the running config store; a nil store
// yields tools that fail cleanly at invoke time (see NewProvider). configPath is
// the bootstrap config.yaml path, which `cfg db migrate` rewrites, and forRole
// is which half of #170's split this process is.
func All(store config.Store, configPath string, forRole config.Role) []tools.Tool {
	// Recorded once for the process. See the note on `role` in deps.go: a broker
	// gateway must be able to name an agent profile whose body lives on a node,
	// which is the one rule the role changes for a `cfg …` write.
	role = forRole
	p := NewProvider(store)
	var out []tools.Tool
	out = append(out, AgentTools(p)...)
	out = append(out, McpTools(p)...)
	out = append(out, JobTools(p)...)
	out = append(out, SingletonTools(p)...)
	out = append(out, RuleTools(p)...)
	out = append(out, AdminTools(p)...)
	out = append(out, DBTools(p, configPath)...)
	out = append(out, NodeSplitTools(p, configPath)...)
	return out
}
