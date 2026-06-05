package ping

import (
	"context"
	"testing"
)

func TestTool_Name(t *testing.T) {
	if got := New().Name(); got != "ping" {
		t.Fatalf("Name() = %q, want %q", got, "ping")
	}
}

func TestTool_InputSchema_IsNil(t *testing.T) {
	if New().InputSchema() != nil {
		t.Fatal("InputSchema() = non-nil, want nil")
	}
}

func TestTool_Invoke_ReturnsPong(t *testing.T) {
	got, err := New().Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
	s, ok := got.(string)
	if !ok {
		t.Fatalf("Invoke returned %T, want string", got)
	}
	if s != "pong" {
		t.Fatalf("Invoke returned %q, want %q", s, "pong")
	}
}
