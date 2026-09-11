package nodeserve

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/nodelink"
)

const signInCallTimeout = 30 * time.Second

// SignIns reaches the node's owner for work with no conversation, such as a
// scheduled job; it outlives connections, so it is bound to whichever serves.
type SignIns struct {
	log *slog.Logger

	mu       sync.Mutex
	server   *Server
	raisedOn map[*agent.SignInPrompt]*Server
}

// NewSignIns refuses every sign-in until a gateway is attached, because until
// then nobody could be asked.
func NewSignIns(log *slog.Logger) *SignIns {
	if log == nil {
		log = slog.Default()
	}
	return &SignIns{log: log, raisedOn: make(map[*agent.SignInPrompt]*Server)}
}

func (r *SignIns) bind(s *Server) {
	r.mu.Lock()
	r.server = s
	r.mu.Unlock()
}

func (r *SignIns) unbind(s *Server) {
	r.mu.Lock()
	if r.server == s {
		r.server = nil
	}
	r.mu.Unlock()
}

func (r *SignIns) SignIn(ctx context.Context, req agent.SignInRequest) (*agent.SignInPrompt, bool) {
	r.mu.Lock()
	server := r.server
	r.mu.Unlock()
	if server == nil {
		r.log.Warn("nodeserve: a sign-in with no conversation was refused; no gateway is attached", "tool", req.Tool)
		return nil, false
	}
	prompt := &agent.SignInPrompt{Request: req, Answer: make(chan agent.DisplayAnswer, 2)}
	if !server.raiseSignIn(ctx, prompt) {
		return nil, false
	}
	r.mu.Lock()
	r.raisedOn[prompt] = server
	r.mu.Unlock()
	return prompt, true
}

// SettleSignIn goes to the connection the sign-in was raised on, because a
// gateway reached later has never heard of it.
func (r *SignIns) SettleSignIn(ctx context.Context, update agent.SignInSettled) {
	r.mu.Lock()
	server := r.raisedOn[update.Prompt]
	if update.State.Terminal() {
		delete(r.raisedOn, update.Prompt)
	}
	r.mu.Unlock()
	if server != nil {
		server.settleSignIn(ctx, update)
	}
}

func (s *Server) raiseSignIn(ctx context.Context, prompt *agent.SignInPrompt) bool {
	wire, _, err := s.enc.Encode(agent.Event{Type: agent.EventSignIn, SignIn: prompt})
	if err != nil {
		s.log.Warn("nodeserve: could not encode a sign-in", "error", err)
		return false
	}
	id := wire.SignIn.ID
	s.registerSignIn(id, "")
	callCtx, cancel := context.WithTimeout(ctx, signInCallTimeout)
	defer cancel()
	if _, err := s.callGateway(callCtx, agentwire.MethodSignIn, wire.SignIn); err != nil {
		s.forget(id)
		s.enc.Abandon(id)
		s.log.Warn("nodeserve: the gateway did not take a sign-in with no conversation", "tool", prompt.Request.Tool, "error", err)
		if callCtx.Err() != nil {
			s.withdrawSignIn(id)
		}
		return false
	}
	return true
}

func (s *Server) withdrawSignIn(id string) {
	ctx, cancel := context.WithTimeout(s.ctx, signInCallTimeout)
	defer cancel()
	_, err := s.callGateway(ctx, agentwire.MethodSignInSettled, agentwire.SignInSettled{ID: id, State: string(agent.SignInCancelled)})
	if err != nil && !errors.Is(err, errLinkGone) && !errors.Is(err, nodelink.ErrLinkClosed) {
		s.log.Warn("nodeserve: could not withdraw a sign-in the gateway may already be showing", "error", err)
	}
}

func (s *Server) settleSignIn(ctx context.Context, update agent.SignInSettled) {
	wire, _, err := s.enc.Encode(agent.Event{Type: agent.EventSignInSettled, SignInSettled: &update})
	if err != nil {
		s.log.Debug("nodeserve: a sign-in settled after its connection forgot it", "state", update.State, "error", err)
		return
	}
	if update.State.Terminal() {
		s.forget(wire.SignInSettled.ID)
	}
	callCtx, cancel := context.WithTimeout(ctx, signInCallTimeout)
	defer cancel()
	_, err = s.callGateway(callCtx, agentwire.MethodSignInSettled, wire.SignInSettled)
	if err != nil && !errors.Is(err, errLinkGone) && !errors.Is(err, nodelink.ErrLinkClosed) {
		s.log.Warn("nodeserve: could not tell the gateway how a sign-in went", "state", update.State, "error", err)
	}
}
