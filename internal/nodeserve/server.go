package nodeserve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/nodelink"
)

var errLinkGone = errors.New("the connection to the gateway ended")

const sendTimeout = 2 * time.Minute

type Options struct {
	Logger     *slog.Logger
	Gate       *ToolGate
	Background *BackgroundSink
	Advertise  *Advertiser

	SignIns *SignIns

	Credentials *Credentials

	// Interruptible is the profile's override; nil falls back to probing the agent, then to true,
	// as in-process, so the gateway is never left to guess.
	Interruptible *bool

	Failed func(error) error

	RenewCredential func(ctx context.Context) (agentwire.CredentialRenewal, error)
	// nil refuses the method: node admins own their node, and this is the one place a gateway can
	// write to one.
	Configure func(ctx context.Context, cfg agentwire.NodeConfiguration) (agentwire.NodeConfigured, error)
	// Fired only after the Configure answer is on the wire, since it cancels this connection; needed
	// because backends latch their tools at startup and cannot gain an agent later.
	Restart func()
	// Zero takes the link's default, which is wrong over a real socket (see nodesocket.DefaultWindowBytes).
	WindowBytes  int
	AckThreshold uint64
	AckInterval  time.Duration
	Epoch        uint64
}

type Server struct {
	client    agent.Client
	log       *slog.Logger
	enc       *agentwire.Encoder
	link      *nodelink.Link
	ready     chan struct{}
	claim     *Advertiser
	configure func(ctx context.Context, cfg agentwire.NodeConfiguration) (agentwire.NodeConfigured, error)
	restart   func()
	failed    func(error) error
	renew     func(ctx context.Context) (agentwire.CredentialRenewal, error)
	override  *bool

	ctx    context.Context
	cancel context.CancelFunc

	nextID atomic.Int64

	mu       sync.Mutex
	turns    map[string]*turn
	asks     map[string]*ask
	calls    map[string]chan agentwire.Message
	headless map[string]bool
}

type turn struct {
	sessionID string
	cancel    context.CancelFunc
}

type ask struct {
	stream  string
	answer  chan agentwire.PermissionResponse
	display bool
	open    bool
}

// Does not close the agent client: the node outlives any one gateway connection, and a reconnect
// must find its backends where it left them.
func Serve(ctx context.Context, conn nodelink.Conn, client agent.Client, opts Options) error {
	if client == nil {
		return errors.New("nodeserve: no agent to serve")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		client:    client,
		log:       log,
		enc:       agentwire.NewEncoder(),
		ready:     make(chan struct{}),
		turns:     make(map[string]*turn),
		asks:      make(map[string]*ask),
		calls:     make(map[string]chan agentwire.Message),
		headless:  make(map[string]bool),
		claim:     opts.Advertise,
		configure: opts.Configure,
		restart:   opts.Restart,
		failed:    opts.Failed,
		renew:     opts.RenewCredential,
		override:  opts.Interruptible,
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	defer s.cancel()

	s.link = nodelink.New(conn, nodelink.Options{
		Handler:      s.consume,
		Logger:       log,
		WindowBytes:  opts.WindowBytes,
		AckThreshold: opts.AckThreshold,
		AckInterval:  opts.AckInterval,
		Epoch:        opts.Epoch,
	})
	close(s.ready)

	if opts.Gate != nil {
		opts.Gate.bind(s)
		defer opts.Gate.unbind(s)
	}
	if opts.Background != nil {
		opts.Background.bind(s)
		defer opts.Background.unbind(s)
	}
	if opts.Advertise != nil {
		opts.Advertise.bind(s)
		defer opts.Advertise.unbind(s)
	}
	if opts.SignIns != nil {
		opts.SignIns.bind(s)
		defer opts.SignIns.unbind(s)
	}
	if opts.Credentials != nil {
		opts.Credentials.bind(s)
		defer opts.Credentials.unbind(s)
	}

	select {
	case <-s.link.Done():
	case <-ctx.Done():
		_ = s.link.Close()
	}
	s.shutdown()

	err := s.link.Err()
	if errors.Is(err, nodelink.ErrLinkClosed) {
		return nil
	}
	return err
}

func (s *Server) consume(payload []byte) error {
	<-s.ready
	msg, err := agentwire.DecodeMessage(payload)
	if err != nil {
		s.log.Warn("nodeserve: undecodable payload from gateway", "error", err)
		return nil
	}
	switch msg.Kind {
	case agentwire.MessageRequest:
		go s.serve(msg)
	case agentwire.MessagePermission:
		s.answer(msg)
	case agentwire.MessageAnswer:
		s.answerDisplay(msg)
	case agentwire.MessageResponse:
		s.deliverResponse(msg)
	default:
		s.log.Warn("nodeserve: unexpected frame from gateway", "kind", msg.Kind, "id", msg.ID)
	}
	return nil
}

func (s *Server) deliverResponse(msg agentwire.Message) {
	s.mu.Lock()
	waiting := s.calls[msg.ID]
	delete(s.calls, msg.ID)
	s.mu.Unlock()
	if waiting == nil {
		s.log.Debug("nodeserve: response for a call nobody is waiting on", "id", msg.ID)
		return
	}
	waiting <- msg
	close(waiting)
}

func (s *Server) callGateway(ctx context.Context, method agentwire.Method, body any) (agentwire.Message, error) {
	id := strconv.FormatInt(s.nextID.Add(1), 10)
	answer := make(chan agentwire.Message, 1)
	s.mu.Lock()
	s.calls[id] = answer
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.calls, id)
		s.mu.Unlock()
	}()

	msg, err := agentwire.Request(id, method, body)
	if err != nil {
		return agentwire.Message{}, err
	}
	if err := s.sendOn(ctx, msg); err != nil {
		return agentwire.Message{}, err
	}

	select {
	case <-ctx.Done():
		select {
		case <-s.link.Done():
			return agentwire.Message{}, errLinkGone
		default:
		}
		return agentwire.Message{}, ctx.Err()
	case <-s.ctx.Done():
		return agentwire.Message{}, errLinkGone
	case <-s.link.Done():
		return agentwire.Message{}, errLinkGone
	case response, delivered := <-answer:
		if !delivered {
			return agentwire.Message{}, errLinkGone
		}
		if fault := response.Fault(); fault != nil {
			return agentwire.Message{}, fault
		}
		return response, nil
	}
}

func (s *Server) failCalls() {
	s.mu.Lock()
	waiting := s.calls
	s.calls = make(map[string]chan agentwire.Message)
	s.mu.Unlock()
	for _, ch := range waiting {
		close(ch)
	}
}

func (s *Server) serve(msg agentwire.Message) {
	switch msg.Method {
	case agentwire.MethodInitialize:
		s.serveInitialize(msg)
	case agentwire.MethodNewSession:
		s.serveNewSession(msg)
	case agentwire.MethodPrompt:
		s.servePrompt(msg)
	case agentwire.MethodCancel:
		s.serveCancel(msg)
	case agentwire.MethodCloseSession:
		s.serveCloseSession(msg)
	case agentwire.MethodConfigure:
		s.serveConfigure(msg)
	case agentwire.MethodRenewCredential:
		s.serveRenewCredential(msg)
	case agentwire.MethodClose:
		s.reply(msg.ID, agentwire.Empty{})
		s.cancel()
		_ = s.link.Close()
	default:
		s.fault(msg.ID, fmt.Errorf("nodeserve: this node serves no %q request", msg.Method))
	}
}

func (s *Server) serveInitialize(msg agentwire.Message) {
	if err := s.client.Initialize(s.ctx); err != nil {
		s.fault(msg.ID, err)
		return
	}
	result := agentwire.InitializeResult{Interruptible: s.interruptible()}
	if s.claim != nil {
		result.Advertisement = s.claim.Current()
	}
	s.replyResult(msg.ID, result)
}

func (s *Server) interruptible() *bool {
	answer := true
	if s.override != nil {
		answer = *s.override
	} else if prober, ok := s.client.(interface {
		SupportsCancel(context.Context) bool
	}); ok {
		answer = prober.SupportsCancel(s.ctx)
	}
	return &answer
}

func (s *Server) serveNewSession(msg agentwire.Message) {
	var meta agentwire.SessionMetadata
	if err := msg.Into(&meta); err != nil {
		s.fault(msg.ID, err)
		return
	}
	decoded := meta.Decode()
	session, err := s.client.NewSession(s.ctx, decoded)
	if err != nil {
		s.fault(msg.ID, err)
		return
	}
	if decoded.Headless {
		s.mu.Lock()
		s.headless[session.ID] = true
		s.mu.Unlock()
	}
	s.replyResult(msg.ID, agentwire.NewSessionResult{SessionID: session.ID})
}

func (s *Server) servePrompt(msg agentwire.Message) {
	var body agentwire.PromptBody
	if err := msg.Into(&body); err != nil {
		s.fault(msg.ID, err)
		return
	}

	turnCtx, cancel := context.WithCancel(s.ctx)
	if !s.isHeadless(body.SessionID) {
		turnCtx = withStream(turnCtx, msg.ID)
	}

	s.mu.Lock()
	s.turns[msg.ID] = &turn{sessionID: body.SessionID, cancel: cancel}
	s.mu.Unlock()

	events, err := s.client.Prompt(turnCtx, body.SessionID, body.Prompt.Decode())
	if err != nil {
		s.endTurn(msg.ID, false)
		s.fault(msg.ID, s.turnFailed(err))
		return
	}
	s.reply(msg.ID, agentwire.Empty{})
	go s.pump(turnCtx, msg.ID, events)
}

func (s *Server) isHeadless(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.headless[sessionID]
}

func (s *Server) serveCancel(msg agentwire.Message) {
	var ref agentwire.SessionRef
	if err := msg.Into(&ref); err != nil {
		s.fault(msg.ID, err)
		return
	}
	if err := s.client.Cancel(s.ctx, ref.SessionID); err != nil {
		s.fault(msg.ID, err)
		return
	}
	s.reply(msg.ID, agentwire.Empty{})
}

func (s *Server) serveCloseSession(msg agentwire.Message) {
	var ref agentwire.SessionRef
	if err := msg.Into(&ref); err != nil {
		s.log.Warn("nodeserve: read session ref", "error", err)
		return
	}
	s.mu.Lock()
	delete(s.headless, ref.SessionID)
	s.mu.Unlock()
	if closer, ok := s.client.(interface{ CloseSession(string) }); ok {
		closer.CloseSession(ref.SessionID)
	}
}

func (s *Server) serveConfigure(msg agentwire.Message) {
	if s.configure == nil {
		s.fault(msg.ID, errors.New("nodeserve: this node does not accept configuration from its gateway"))
		return
	}
	var cfg agentwire.NodeConfiguration
	if err := msg.Into(&cfg); err != nil {
		s.fault(msg.ID, fmt.Errorf("nodeserve: read the configuration: %w", err))
		return
	}
	result, err := s.configure(s.ctx, cfg)
	if err != nil {
		s.fault(msg.ID, err)
		return
	}
	s.log.Info("this node was configured by its gateway", "profiles", result.Applied, "restarting", result.Restarting)
	s.reply(msg.ID, result)
	if result.Restarting && s.restart != nil {
		s.restart()
	}
}

func (s *Server) answer(msg agentwire.Message) {
	var resp agentwire.PermissionResponse
	if err := msg.Into(&resp); err != nil {
		s.log.Warn("nodeserve: read permission answer", "error", err)
		return
	}
	s.deliver(resp)
}

func (s *Server) deliver(resp agentwire.PermissionResponse) {
	s.mu.Lock()
	pending := s.asks[resp.ID]
	delete(s.asks, resp.ID)
	s.mu.Unlock()
	if pending == nil {
		s.log.Debug("nodeserve: permission answer for an unknown request", "id", resp.ID)
		return
	}
	if pending.answer != nil {
		select {
		case pending.answer <- resp:
		default:
		}
		return
	}
	if err := s.enc.Resolve(resp); err != nil {
		s.log.Warn("nodeserve: deliver permission answer", "error", err, "id", resp.ID)
	}
}

func (s *Server) answerDisplay(msg agentwire.Message) {
	var answer agentwire.DisplayAnswer
	if err := msg.Into(&answer); err != nil {
		s.log.Warn("nodeserve: read display answer", "error", err)
		return
	}
	s.deliverDisplay(answer)
}

func (s *Server) deliverDisplay(answer agentwire.DisplayAnswer) {
	s.mu.Lock()
	pending := s.asks[answer.ID]
	if pending != nil && !pending.open {
		delete(s.asks, answer.ID)
	}
	s.mu.Unlock()
	if pending == nil {
		s.log.Debug("nodeserve: display answer for an unknown request", "id", answer.ID)
		return
	}
	if err := s.enc.Answer(answer); err != nil {
		s.log.Warn("nodeserve: deliver display answer", "error", err, "id", answer.ID)
	}
}

func (s *Server) register(id, stream string, answer chan agentwire.PermissionResponse) {
	s.mu.Lock()
	s.asks[id] = &ask{stream: stream, answer: answer}
	s.mu.Unlock()
}

func (s *Server) sessionOf(stream string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.turns[stream]; t != nil {
		return t.sessionID
	}
	return ""
}

func (s *Server) registerDisplay(id, stream string) {
	s.mu.Lock()
	s.asks[id] = &ask{stream: stream, display: true}
	s.mu.Unlock()
}

func (s *Server) registerSignIn(id, stream string) {
	s.mu.Lock()
	s.asks[id] = &ask{stream: stream, display: true, open: true}
	s.mu.Unlock()
}

func (s *Server) forget(id string) {
	s.mu.Lock()
	delete(s.asks, id)
	s.mu.Unlock()
}

func (s *Server) dismissTurnAsks(stream string) {
	s.mu.Lock()
	dismissals := make([]agentwire.PermissionResponse, 0, len(s.asks))
	var displays []agentwire.DisplayAnswer
	var signIns []string
	for id, pending := range s.asks {
		if pending.stream != stream {
			continue
		}
		if pending.display {
			displays = append(displays, agentwire.DisplayAnswer{ID: id, Outcome: string(agent.DisplayDismissed)})
			if pending.open {
				signIns = append(signIns, id)
			}
			continue
		}
		resp := agentwire.PermissionResponse{ID: id}
		if pending.answer != nil {
			resp.Note = "Skipped: the turn ended before the approval was answered. The action was not run."
		}
		dismissals = append(dismissals, resp)
	}
	s.mu.Unlock()
	for _, resp := range dismissals {
		s.deliver(resp)
	}
	for _, answer := range displays {
		s.deliverDisplay(answer)
	}
	for _, id := range signIns {
		s.forget(id)
		s.enc.Abandon(id)
	}
}

func (s *Server) endTurn(stream string, sendEnd bool) {
	s.mu.Lock()
	t := s.turns[stream]
	delete(s.turns, stream)
	s.mu.Unlock()
	if t == nil {
		return
	}
	s.dismissTurnAsks(stream)
	if sendEnd {
		s.send(agentwire.StreamEnd(stream))
	}
	t.cancel()
}

func (s *Server) shutdown() {
	s.mu.Lock()
	streams := make([]string, 0, len(s.turns))
	for id := range s.turns {
		streams = append(streams, id)
	}
	s.mu.Unlock()
	for _, id := range streams {
		s.endTurn(id, false)
	}
	s.dismissTurnAsks("")
	s.failCalls()
	s.cancel()
}

func (s *Server) serveRenewCredential(msg agentwire.Message) {
	if s.renew == nil {
		s.fault(msg.ID, errors.New("nodeserve: this node has no credential it can sign in again"))
		return
	}
	renewal, err := s.renew(s.ctx)
	if err != nil {
		s.fault(msg.ID, err)
		return
	}
	s.reply(msg.ID, renewal)
}

func (s *Server) turnFailed(err error) error {
	if s.failed == nil || err == nil {
		return err
	}
	return s.failed(err)
}

func (s *Server) reply(id string, body any) {
	msg, err := agentwire.Result(id, body)
	if err != nil {
		s.fault(id, err)
		return
	}
	s.send(msg)
}

func (s *Server) replyResult(id string, body any) { s.reply(id, body) }

func (s *Server) fault(id string, err error) {
	s.log.Warn("nodeserve: request failed", "id", id, "error", err)
	s.send(agentwire.Fault(id, err))
}

func (s *Server) send(msg agentwire.Message) {
	raw, err := msg.Encode()
	if err != nil {
		s.log.Warn("nodeserve: encode frame", "kind", msg.Kind, "error", err)
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, sendTimeout)
	defer cancel()
	if err := s.link.Send(ctx, raw); err != nil && !errors.Is(err, nodelink.ErrLinkClosed) {
		s.log.Warn("nodeserve: send frame", "kind", msg.Kind, "error", err)
	}
}

func (s *Server) sendOn(ctx context.Context, msg agentwire.Message) error {
	raw, err := msg.Encode()
	if err != nil {
		return err
	}
	return s.link.Send(ctx, raw)
}
