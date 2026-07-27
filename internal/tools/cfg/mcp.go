package cfg

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/tools"
)

// mcpSetTool creates or updates one MCP server from typed flags. Exactly one
// transport is used (a stdio child process via --command/--arg/--env, or a
// remote endpoint via --url); Validate enforces that, so upsertItemValidated
// rejects a half-set entry and rolls the store back.
type mcpSetTool struct{ p Provider }

func (t *mcpSetTool) Name() string { return "cfg.mcp.set" }
func (t *mcpSetTool) Description() string {
	return "Create or update an MCP server (e.g. `cfg mcp set --name fs --command npx --arg -y --arg @modelcontextprotocol/server-filesystem`)."
}
func (t *mcpSetTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name":    {Type: "string", Description: "MCP server name (the key it is stored under)"},
			"command": {Type: "string", Description: "stdio transport: the process command"},
			"arg":     {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "stdio process argument (repeatable)"},
			"env":     {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "process env var KEY=VALUE (repeatable)"},
			"url":     {Type: "string", Description: "remote transport: the server endpoint URL"},
		},
	}
}
func (t *mcpSetTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	name, err := requireString(args, "name")
	if err != nil {
		return nil, err
	}
	s, err := t.p()
	if err != nil {
		return nil, err
	}
	var cfg config.MCPServerConfig
	if body, ok, err := s.GetItem(ctx, config.SectionMCP, name); err != nil {
		return nil, err
	} else if ok {
		if err := json.Unmarshal(body, &cfg); err != nil {
			return nil, err
		}
	}
	if v, ok := stringArg(args, "command"); ok {
		cfg.Command = v
	}
	if v, ok := arrayArg(args, "arg"); ok {
		cfg.Args = v
	}
	if env, ok, err := envArg(args, "env"); err != nil {
		return nil, err
	} else if ok {
		cfg.Env = env
	}
	if v, ok := stringArg(args, "url"); ok {
		cfg.URL = v
	}
	if err := upsertItemValidated(ctx, s, config.SectionMCP, name, cfg); err != nil {
		return nil, err
	}
	return okResult{Message: fmt.Sprintf("saved MCP server %q", name)}, nil
}

// McpTools returns the MCP set tool plus the shared list/show/delete trio.
func McpTools(p Provider) []tools.Tool {
	out := []tools.Tool{&mcpSetTool{p: p}}
	return append(out, sectionTools(p, config.SectionMCP, "cfg.mcp", "MCP server")...)
}
