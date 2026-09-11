package nodeserve

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/nodelink"
)

// Unbound, it drops events: with no gateway attached the session id means nothing, and queueing
// would pile up notices about work that finished long before anything reconnected.
type BackgroundSink struct {
	log *slog.Logger

	mu     sync.Mutex
	server *Server
}

func NewBackgroundSink(log *slog.Logger) *BackgroundSink {
	if log == nil {
		log = slog.Default()
	}
	return &BackgroundSink{log: log}
}

func (b *BackgroundSink) bind(s *Server) {
	b.mu.Lock()
	b.server = s
	b.mu.Unlock()
}

func (b *BackgroundSink) unbind(s *Server) {
	b.mu.Lock()
	if b.server == s {
		b.server = nil
	}
	b.mu.Unlock()
}

func (b *BackgroundSink) bound() *Server {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.server
}

// Called from the backend's own goroutine, never the link's read loop, so blocking on a full window
// stalls only the session that produced the event.
func (b *BackgroundSink) Handle(sessionID string, ev agent.Event) {
	server := b.bound()
	if server == nil {
		b.log.Debug("nodeserve: dropping a background event; no gateway is attached", "session_id", sessionID, "type", string(ev.Type))
		return
	}
	server.sendBackground(sessionID, ev)
}

func (s *Server) sendBackground(sessionID string, ev agent.Event) {
	if sessionID == "" {
		s.log.Warn("nodeserve: a background event named no session", "type", string(ev.Type))
		return
	}
	if ev.Type == agent.EventPermission {
		if ev.Permission != nil && ev.Permission.Decision != nil {
			select {
			case ev.Permission.Decision <- "":
			default:
			}
		}
		s.log.Warn("nodeserve: refusing to send an approval request with no turn to answer it", "session_id", sessionID)
		return
	}

	if answers := displayAnswers(ev); answers != nil {
		select {
		case answers <- agent.DisplayAnswer{Outcome: agent.DisplayNoConversation}:
		default:
		}
		s.log.Warn("nodeserve: refusing a question, plan or sign-in raised with no turn to answer it", "session_id", sessionID)
		return
	}

	ctx, cancel := context.WithTimeout(s.ctx, sendTimeout)
	defer cancel()

	wire, transfer, err := s.enc.Encode(ev)
	if err != nil {
		s.log.Warn("nodeserve: could not encode a background event", "session_id", sessionID, "type", string(ev.Type), "error", err)
		return
	}
	if transfer != nil {
		if err := s.sendTransfer(ctx, transfer); err != nil {
			s.log.Warn("nodeserve: could not transfer a background attachment", "session_id", sessionID, "error", err)
			return
		}
	}
	msg, err := agentwire.BackgroundEvent(sessionID, wire)
	if err != nil {
		s.log.Warn("nodeserve: could not build a background frame", "session_id", sessionID, "error", err)
		return
	}
	if err := s.sendOn(ctx, msg); err != nil && !errors.Is(err, nodelink.ErrLinkClosed) {
		s.log.Warn("nodeserve: could not send a background event", "session_id", sessionID, "error", err)
	}
}

var _ = agentruntime.Hooks{BackgroundEvents: (*BackgroundSink)(nil).Handle}

func displayAnswers(ev agent.Event) chan agent.DisplayAnswer {
	switch {
	case ev.Question != nil:
		return ev.Question.Answer
	case ev.Plan != nil:
		return ev.Plan.Answer
	case ev.SignIn != nil:
		return ev.SignIn.Answer
	}
	return nil
}
