package nodeserve

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// The gate's two "nobody can be asked" branches are the ones worth pinning,
// because both are silent and they point in opposite directions. The rest of
// this package is exercised end to end over a real socket by
// internal/nodehost's loopback tests.

// No turn on the context means no gateway is watching this call — a delegated
// run, a job, an unfurl. In process those are built with no approver at all and
// run ungated, so the gate must not invent a refusal for them.
func TestAnUngatedCallOutsideATurnIsAllowed(t *testing.T) {
	gate := NewToolGate(nil)

	allowed, note := gate.Approve(context.Background(), "terminal", "ls")

	if !allowed {
		t.Fatalf("a call outside a turn was refused with %q; delegated runs are ungated in process and must stay so", note)
	}
	if note != "" {
		t.Fatalf("an allowed call carried a note: %q", note)
	}
}

// An unbound gate means a turn is in flight but the connection to the gateway
// is gone. That must DENY: the request exists because the call is
// side-effecting, and "nobody could be asked" is not consent. The note matters
// as much as the verdict — it is the string the model is handed as the call's
// result.
func TestATurnWithNoConnectionIsDeniedWithAReason(t *testing.T) {
	gate := NewToolGate(nil)

	allowed, note := gate.Approve(withStream(context.Background(), "turn-1"), "terminal", "rm -rf /")

	if allowed {
		t.Fatal("a side-effecting call was allowed while no gateway was attached")
	}
	if note == "" {
		t.Fatal("a refused call carried no note, so the model is told nothing about why it did not run")
	}
}

// discardLogger keeps the harness quiet; what these tests assert is behaviour,
// not log output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
