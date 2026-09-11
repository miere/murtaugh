package nodeserve

import (
	"context"
	"log/slog"
	"sync"

	"github.com/google/uuid"

	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/agentwire"
)

// ToolGate is the node's inline tool-approval gate: the thing a native agent's
// loop calls where, in process, it would call the gateway's own approver.
//
// It is built before any connection exists because agentbuild wants an Approver
// at construction, and it is bound to whichever server is currently serving.
// Unbound it DENIES, with a note saying why. That is the deliberate choice: an
// unbound gate means no gateway is attached, so nobody can be asked, and a
// side-effecting tool call that runs because the answer could not be requested
// is exactly the failure the gate exists to prevent. A delegated run — a job, a
// workflow trigger — never reaches here at all, because it is built with no
// approver, the same as in process.
type ToolGate struct {
	log *slog.Logger

	mu     sync.Mutex
	server *Server
}

// NewToolGate returns an unbound gate.
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

// Approve asks the gateway's human and returns what the loop expects.
//
// The note is not diagnostics. For a native tool call it becomes the call's
// result string, handed to the model as the reason the action did not happen,
// so every branch below returns one that reads as an answer rather than as an
// error.
func (g *ToolGate) Approve(ctx context.Context, toolName, summary string) (bool, string) {
	stream, ok := streamOf(ctx)
	if !ok {
		// No turn: this call is not part of anything a gateway is watching, so
		// there is no thread to post a card into. Ungated, which is exactly what
		// the gateway's own approver does with no TurnLocation on the context.
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

// streamKey carries the turn's stream id on the context the backend runs under.
type streamKey struct{}

func withStream(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, streamKey{}, id)
}

func streamOf(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(streamKey{}).(string)
	return id, ok && id != ""
}

// The gate is the node's implementation of the runtime's approval seam, so a
// backend cannot tell whether the human it is waiting on is one process away or
// one network away.
var _ agentruntime.Approver = (*ToolGate)(nil)
