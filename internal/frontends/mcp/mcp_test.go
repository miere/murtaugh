package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
)

type fakeTool struct {
	name   string
	schema *jsonschema.Schema
	result any
}

func (f *fakeTool) Name() string                    { return f.name }
func (f *fakeTool) Description() string             { return "fake tool" }
func (f *fakeTool) InputSchema() *jsonschema.Schema { return f.schema }
func (f *fakeTool) Invoke(_ context.Context, _ map[string]any) (any, error) {
	return f.result, nil
}

func newConnectedClient(t *testing.T, f *Frontend) *mcpsdk.ClientSession {
	t.Helper()
	ctx := context.Background()

	serverTransport, clientTransport := mcpsdk.NewInMemoryTransports()
	server := f.Server()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server.Connect: %v", err)
	}

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "v0.0.0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestServer_ListsRegisteredTool(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{name: "ping", result: "pong"})

	session := newConnectedClient(t, New(reg))

	res, err := session.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Name != "ping" {
		t.Fatalf("ListTools = %+v, want one ping tool", res.Tools)
	}
}

func TestServer_CallTool_StringResult_PassesThrough(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{name: "ping", result: "pong"})

	session := newConnectedClient(t, New(reg))

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "ping"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned error result: %+v", res)
	}
	text, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("CallTool content = %T, want *TextContent", res.Content[0])
	}
	if text.Text != "pong" {
		t.Fatalf("CallTool text = %q, want %q", text.Text, "pong")
	}
}

func TestServer_CallTool_StructResult_JSONMarshalled(t *testing.T) {
	type out struct {
		OK   bool   `json:"ok"`
		Name string `json:"name"`
	}
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{
		name: "jobs.run",
		schema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"name": {Type: "string"}},
			Required:   []string{"name"},
		},
		result: out{OK: true, Name: "demo"},
	})
	session := newConnectedClient(t, New(reg))

	res, err := session.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name:      "jobs.run",
		Arguments: map[string]any{"name": "demo"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool returned error result: %+v", res)
	}
	text := res.Content[0].(*mcpsdk.TextContent).Text
	var got out
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("result text not valid JSON: %v; text=%q", err, text)
	}
	if got != (out{OK: true, Name: "demo"}) {
		t.Fatalf("decoded = %+v, want {OK:true Name:demo}", got)
	}
}

func TestServer_PublishesInputSchema(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(&fakeTool{
		name: "jobs.run",
		schema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"name": {Type: "string"}},
			Required:   []string{"name"},
		},
		result: "ok",
	})
	session := newConnectedClient(t, New(reg))

	res, err := session.ListTools(context.Background(), &mcpsdk.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	raw, err := json.Marshal(res.Tools[0].InputSchema)
	if err != nil {
		t.Fatalf("marshal input schema: %v", err)
	}
	var got struct {
		Type     string   `json:"type"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v; raw=%s", err, string(raw))
	}
	if got.Type != "object" {
		t.Fatalf("schema type = %q, want object", got.Type)
	}
	if len(got.Required) != 1 || got.Required[0] != "name" {
		t.Fatalf("required = %v, want [name]", got.Required)
	}
}
