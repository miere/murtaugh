// Package mcp implements the MCP (stdio) frontend. It exposes every tool in
// the shared registry as an MCP tool, and runs the JSON-RPC server over
// stdin/stdout. Stdout is reserved for protocol messages; no logs or raw
// text are written to it.
//
// Output convention: a tool's result is JSON-marshalled and wrapped in a
// single TextContent block. A plain string result is passed through as-is so
// trivial tools (e.g. ping) don't need a struct.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
)

// ServerName is advertised to MCP clients.
const ServerName = "murtaugh-mcp"

// ServerVersion is advertised to MCP clients.
const ServerVersion = "0.1.0"

// Frontend is the MCP adapter.
type Frontend struct {
	registry *tools.Registry
}

// New constructs an MCP Frontend backed by the given registry.
func New(reg *tools.Registry) *Frontend {
	return &Frontend{registry: reg}
}

// Server builds an *mcpsdk.Server with every registered tool wired in. It is
// exposed so tests can inspect the resulting server without touching stdio.
func (f *Frontend) Server() *mcpsdk.Server {
	s := mcpsdk.NewServer(&mcpsdk.Implementation{
		Name:    ServerName,
		Version: ServerVersion,
	}, nil)
	// Guard against two registry keys collapsing onto the same published name
	// (e.g. "a.b" and "a-b" would both sanitise to "a_b"). The MCP SDK silently
	// shadows a duplicate rather than erroring, so we fail loudly at startup —
	// a collision is a programming error in the tool registry, not a runtime
	// condition. None exist today; this keeps it that way.
	all := f.registry.All()
	seen := make(map[string]string, len(all))
	for _, t := range all {
		published := mcpToolName(t.Name())
		if prior, dup := seen[published]; dup {
			panic(fmt.Sprintf("mcp: tool name collision: %q and %q both publish as %q", prior, t.Name(), published))
		}
		seen[published] = t.Name()
		registerTool(s, t)
	}
	return s
}

// invalidMCPNameChar matches any rune disallowed in an LLM-facing tool name.
// The MCP Go SDK tolerates '.', but stricter providers reject it — Gemini's
// function-name regex is exactly [a-zA-Z0-9_-]+, so a dotted name like
// "jobs.define" (after Goose namespacing, "murtaugh__jobs.define") is refused
// with a -32600 "invalid characters" error and the tool call never runs.
var invalidMCPNameChar = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// mcpToolName normalises a registry tool name into an identifier safe to
// expose over MCP: every character outside [A-Za-z0-9_-] becomes '_', so
// "jobs.define" is published as "jobs_define". This is done here, at the single
// boundary where the dotted registry key becomes the LLM-facing id, rather than
// renaming the tools themselves — the dotted keys stay load-bearing for the CLI
// ("murtaugh jobs define") and help. Dispatch is unaffected: AddTool keys the
// handler on the published name, so an inbound CallTool carrying "jobs_define"
// matches and invokes the captured tool directly.
func mcpToolName(name string) string {
	return invalidMCPNameChar.ReplaceAllString(name, "_")
}

// Serve runs the MCP server over a stdio transport. It blocks until the
// connected client disconnects or ctx is cancelled.
func (f *Frontend) Serve(ctx context.Context) error {
	return f.Server().Run(ctx, &mcpsdk.StdioTransport{})
}

// registerTool wires a single tools.Tool into the MCP server using the
// low-level Server.AddTool, so we can publish the tool's own InputSchema and
// dispatch dynamic map[string]any arguments.
func registerTool(s *mcpsdk.Server, t tools.Tool) {
	schema := t.InputSchema()
	if schema == nil {
		schema = emptyObjectSchema()
	}
	handler := func(ctx context.Context, req *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		args, err := decodeArgs(req.Params.Arguments)
		if err != nil {
			return errorResult(err), nil
		}
		result, err := t.Invoke(ctx, args)
		if err != nil {
			return errorResult(err), nil
		}
		text, err := renderJSON(result)
		if err != nil {
			return errorResult(err), nil
		}
		return &mcpsdk.CallToolResult{
			Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
		}, nil
	}
	s.AddTool(&mcpsdk.Tool{
		Name:        mcpToolName(t.Name()),
		Description: t.Description(),
		InputSchema: schema,
	}, handler)
}

// emptyObjectSchema returns the canonical {"type":"object"} schema the SDK
// requires for tools that take no parameters.
func emptyObjectSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object"}
}

// decodeArgs unmarshals raw JSON arguments into a map. An empty payload
// yields a nil map so tools don't need to nil-check.
func decodeArgs(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	return m, nil
}

// renderJSON encodes a tool result for transport. Strings are passed through
// to keep trivial tools' output uncluttered; everything else is JSON.
func renderJSON(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// errorResult builds the CallToolResult shape used to surface tool errors,
// per the MCP convention of returning IsError=true with a TextContent
// payload.
func errorResult(err error) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		IsError: true,
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: err.Error()}},
	}
}
