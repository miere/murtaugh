package nodeserve

import (
	"context"
	"log/slog"
	"sync"

	"github.com/google/uuid"

	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/agentwire"
)

// Unbound, it denies: no gateway is attached so nobody can be asked, and running a side-effecting
// tool unasked is exactly what the gate exists to prevent.
type ToolGate struct {
	log *slog.Logger

	mu     sync.Mutex
	server *Server
}

func NewToolGate(log *slog.Logger) *ToolGate {
	if log == nil {
		log = slog.Default()
	}
	return &ToolGate{log: log}
}

func (g *ToolGate) bind(s *Server) {
	g.mu.Lock()
	g.server = s
	g.mu.Unlock()
}

func (g *ToolGate) unbind(s *Server) {
	g.mu.Lock()
	if g.server == s {
		g.server = nil
	}
	g.mu.Unlock()
}

func (g *ToolGate) bound() *Server {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.server
}

// The note is not diagnostics: for a native tool call it becomes the result handed to the model,
// so every note must read as an answer, not an error.
func (g *ToolGate) Approve(ctx context.Context, toolName, summary string) (bool, string) {
	stream, ok := streamOf(ctx)
	if !ok {
		return true, ""
	}
	server := g.bound()
	if server == nil {
		return false, "Skipped: the connection to Slack is down, so nobody could be asked to approve this. The action was not run."
	}

	id := uuid.NewString()
	answer := make(chan agentwire.PermissionResponse, 1)
	server.register(id, stream, answer)

	request := agentwire.NativeApproval(id, toolName, summary)
	request.SessionID = server.sessionOf(stream)
	msg, err := agentwire.StreamEvent(stream, agentwire.Event{
		Type:       agentwire.EventPermission,
		Permission: &request,
	})
	if err != nil {
		server.forget(id)
		return false, "Skipped: the approval request could not be sent. The action was not run."
	}
	if err := server.sendOn(ctx, msg); err != nil {
		server.forget(id)
		g.log.Warn("nodeserve: could not raise an approval request", "tool", toolName, "error", err)
		return false, "Skipped: the approval request could not be sent. The action was not run."
	}

	select {
	case resp := <-answer:
		return resp.Approval()
	case <-ctx.Done():
		server.forget(id)
		return false, "Skipped: the turn was interrupted before the approval was answered. The action was not run."
	case <-server.ctx.Done():
		server.forget(id)
		return false, "Skipped: the connection to Slack dropped before the approval was answered. The action was not run."
	}
}

type streamKey struct{}

func withStream(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, streamKey{}, id)
}

func streamOf(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(streamKey{}).(string)
	return id, ok && id != ""
}

var _ agentruntime.Approver = (*ToolGate)(nil)
