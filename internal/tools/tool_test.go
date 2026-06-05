package tools

import (
	"context"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

type stubTool struct {
	name string
}

func (s stubTool) Name() string                                      { return s.name }
func (stubTool) Description() string                                 { return "stub" }
func (stubTool) InputSchema() *jsonschema.Schema                     { return nil }
func (stubTool) Invoke(context.Context, map[string]any) (any, error) { return nil, nil }

func TestRegistry_Get_ReturnsFalseForUnknown(t *testing.T) {
	r := NewRegistry()
	if _, ok := r.Get("missing"); ok {
		t.Fatal("Get returned ok=true for unregistered tool")
	}
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool{name: "ping"})

	got, ok := r.Get("ping")
	if !ok {
		t.Fatal("Get returned ok=false for registered tool")
	}
	if got.Name() != "ping" {
		t.Fatalf("Name() = %q, want %q", got.Name(), "ping")
	}
}

func TestRegistry_All_PreservesRegistrationOrder(t *testing.T) {
	r := NewRegistry()
	for _, n := range []string{"c", "a", "b"} {
		r.Register(stubTool{name: n})
	}
	want := []string{"c", "a", "b"}
	all := r.All()
	if len(all) != len(want) {
		t.Fatalf("All() len = %d, want %d", len(all), len(want))
	}
	for i, t2 := range all {
		if t2.Name() != want[i] {
			t.Fatalf("All()[%d].Name() = %q, want %q", i, t2.Name(), want[i])
		}
	}
}

func TestRegistry_Register_PanicsOnDuplicate(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	r := NewRegistry()
	r.Register(stubTool{name: "ping"})
	r.Register(stubTool{name: "ping"})
}
