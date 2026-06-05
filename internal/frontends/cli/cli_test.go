package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
)

// fakeTool is a parameterised tool used to exercise nested routing, flag
// parsing and arg passthrough from a single place.
type fakeTool struct {
	name   string
	schema *jsonschema.Schema
	got    map[string]any
	result any
}

func (f *fakeTool) Name() string                    { return f.name }
func (f *fakeTool) Description() string             { return "fake tool" }
func (f *fakeTool) InputSchema() *jsonschema.Schema { return f.schema }
func (f *fakeTool) Invoke(_ context.Context, args map[string]any) (any, error) {
	f.got = args
	return f.result, nil
}

func newTestFrontend(t *testing.T, tl tools.Tool) (*Frontend, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	reg := tools.NewRegistry()
	reg.Register(tl)
	var stdout, stderr bytes.Buffer
	return New(reg).WithOutput(&stdout, &stderr), &stdout, &stderr
}

func TestRun_NoArgs_ReturnsUsageError(t *testing.T) {
	reg := tools.NewRegistry()
	var stdout, stderr bytes.Buffer
	f := New(reg).WithOutput(&stdout, &stderr)
	if err := f.Run(context.Background(), nil); err == nil {
		t.Fatal("Run returned nil, want error")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRun_UnknownCommand_ReturnsError(t *testing.T) {
	reg := tools.NewRegistry()
	var stdout, stderr bytes.Buffer
	f := New(reg).WithOutput(&stdout, &stderr)
	err := f.Run(context.Background(), []string{"nope"})
	if err == nil {
		t.Fatal("Run returned nil, want error")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("error = %v, want it to mention 'unknown command'", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestRun_FlatTool_RendersResult(t *testing.T) {
	tl := &fakeTool{name: "ping", result: "pong"}
	f, stdout, stderr := newTestFrontend(t, tl)
	if err := f.Run(context.Background(), []string{"ping"}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := stdout.String(); got != "pong\n" {
		t.Fatalf("stdout = %q, want %q", got, "pong\n")
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestRun_NestedNamespace_ResolvesDottedTool(t *testing.T) {
	tl := &fakeTool{
		name: "jobs.run",
		schema: &jsonschema.Schema{
			Type:       "object",
			Properties: map[string]*jsonschema.Schema{"name": {Type: "string"}},
		},
		result: "job 'demo' completed",
	}
	f, stdout, _ := newTestFrontend(t, tl)

	if err := f.Run(context.Background(), []string{"jobs", "run", "--name", "demo"}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got := stdout.String(); got != "job 'demo' completed\n" {
		t.Fatalf("stdout = %q, want %q", got, "job 'demo' completed\n")
	}
	if got, want := tl.got["name"], "demo"; got != want {
		t.Fatalf("tool got name=%v, want %v", got, want)
	}
}

func TestRun_PassesParsedArgs_ToTool(t *testing.T) {
	tl := &fakeTool{
		name: "echo",
		schema: &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"attachment_type": {Type: "string"},
			},
		},
		result: "ok",
	}
	f, _, _ := newTestFrontend(t, tl)

	if err := f.Run(context.Background(), []string{"echo", "--attachment-type", "markdown"}); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if got, want := tl.got["attachment_type"], "markdown"; got != want {
		t.Fatalf("tool got attachment_type=%v, want %v", got, want)
	}
}
