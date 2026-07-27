package cfg

import (
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/tools"
)

// All returns every `cfg …` tool bound to the given store. The composition root
// registers them all so the admin surface is exposed identically over the CLI
// and MCP. Pass a store obtained from the running config store; a nil store
// yields tools that fail cleanly at invoke time (see NewProvider).
func All(store config.Store) []tools.Tool {
	p := NewProvider(store)
	var out []tools.Tool
	out = append(out, AgentTools(p)...)
	out = append(out, McpTools(p)...)
	out = append(out, JobTools(p)...)
	out = append(out, SingletonTools(p)...)
	out = append(out, RuleTools(p)...)
	out = append(out, AdminTools(p)...)
	return out
}
