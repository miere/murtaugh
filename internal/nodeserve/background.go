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

// BackgroundSink is the node's outbound path for an event that belongs to a
// SESSION rather than to a turn: a claude_code subagent finishing after its
// turn's `result`, or the auto-continue that completes minutes later. It is the
// producing end of what the gateway renders as the "went quiet" notice.
//
// It exists for the same reason ToolGate does, and has the same shape. The
// backend captures its sink at construction (agentbuild.Deps.BackgroundSink,
// held by the claude_code session for the life of its process) while the
// connection it writes to comes and goes, so the node builds one sink, hands it
// to the runtime, and binds whichever server is currently serving.
//
// Unbound it DROPS, with a log line. No gateway is attached, so the session id
// the event is addressed by means nothing to anyone and there is no thread to
// render into; queueing instead would grow an unbounded backlog of notices
// about stretches that finished hours before anything reconnected.
type BackgroundSink struct {
	log *slog.Logger

	mu     sync.Mutex
	server *Server
}

// NewBackgroundSink returns an unbound sink.
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

// Handle puts one session-addressed event on the wire.
//
// It is agentruntime.Hooks.BackgroundEvents on a node, and it is called from
// the backend's own goroutine — claude_code's stdout reader for that session —
// never from the link's read loop. So parking on a full window stalls the one
// session that produced the event, which is the same backpressure a turn's
// events get, and not frame delivery for the connection.
func (b *BackgroundSink) Handle(sessionID string, ev agent.Event) {
	server := b.bound()
	if server == nil {
		b.log.Debug("nodeserve: dropping a background event; no gateway is attached", "session_id", sessionID, "type", string(ev.Type))
		return
	}
	server.sendBackground(sessionID, ev)
}

// sendBackground encodes one background event and sends it addressed by session.
func (s *Server) sendBackground(sessionID string, ev agent.Event) {
	if sessionID == "" {
		// The gateway drops an event addressed to neither a turn nor a session,
		// so sending one would be a frame's worth of nothing.
		s.log.Warn("nodeserve: a background event named no session", "type", string(ev.Type))
		return
	}
	if ev.Type == agent.EventPermission {
		// A permission is a REQUEST and a background frame has no stream for the
		// answer to come back on. Neither backend can raise one outside a turn —
		// claude_code denies with no active turn and the native loop calls the
		// Approver inline — so this is a guard, not a path. It answers the
		// prompt rather than encoding it, because encoding would register a
		// correlation id nothing will ever resolve and park the goroutine that
		// raised it until the agent is killed.
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
		// Nowhere to report it: there is no turn to put an error event on, and
		// the gateway would render one against a session it is not prompting.
		s.log.Warn("nodeserve: could not encode a background event", "session_id", sessionID, "type", string(ev.Type), "error", err)
		return
	}
	if transfer != nil {
		// Bytes first, event second — the same ordering a turn's attachment
		// uses and for the same reason: the gateway materialises it on the read
		// loop the chunks arrive on.
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

// Compile-time proof that this is the runtime's background seam and not a
// look-alike: Handle is what a gateway would pass in process, so a backend
// cannot tell whether the thread its notice lands in is one process away or one
// network away. The method value is never called — it only has to typecheck.
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
