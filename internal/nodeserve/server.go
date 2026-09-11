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

// sendTimeout bounds one outbound frame's wait for window room. It is long
// because the window only stays shut while the gateway is genuinely not
// consuming — a Slack upload, or a human staring at an approval card — and
// short enough that a gateway which has stopped consuming forever is noticed.
const sendTimeout = 2 * time.Minute

// Options configures a served connection.
type Options struct {
	Logger *slog.Logger
	// Gate is the node's inline tool-approval gate. It is built before the
	// agent (agentbuild wants it at construction) and bound to this server for
	// as long as the connection lasts. nil leaves native tool calls ungated,
	// which is what a node with no gateway attached already is.
	Gate *ToolGate
	// Background is the node's outbound path for an event addressed to a
	// session rather than to a turn — the claude_code background stretch the
	// gateway renders a "went quiet" notice for. Like the gate it is built
	// before the agent and bound to this server for as long as the connection
	// lasts. nil drops them, which is what a node whose runtime was built with
	// no background hook already does.
	Background *BackgroundSink
	// Tools is the node's proxy for Murtaugh's own tools, which live on the
	// gateway. Like the gate it is built before the agent — the agent's toolset
	// is resolved and LATCHED at its first Initialize, so a proxy populated
	// after that point would be a registry the agent never looks at again.
	//
	// nil leaves the node's agent with its backend's own tools and none of
	// Murtaugh's, which is what every node did before #194.
	Tools *ToolProxy
	// Advertise holds what this node claims to serve. Like the three above it
	// is built before the agent and bound for as long as the connection lasts,
	// but it is read at a specific moment rather than called back into: the
	// handshake answer carries whatever it holds, and every later change is
	// pushed through it.
	//
	// nil advertises nothing, which is what every node did before #195 and is
	// indistinguishable to the gateway from a node that has never been
	// configured.
	Advertise *Advertiser
	// Configure applies the agent profiles a Slack onboarding form produced for
	// this node's owner, into this node's OWN store.
	//
	// nil refuses the method, which is the right answer for any node that did
	// not opt in: #170 says node admins own their node, and this is the one
	// place a gateway can write to one. The node's own applier is where the
	// "only while unconfigured" guarantee is enforced — see
	// agentwire.NodeConfiguration.
	Configure func(ctx context.Context, cfg agentwire.NodeConfiguration) (agentwire.NodeConfigured, error)
	// Restart stops the node so its supervisor brings it back up serving what it
	// was just given. Called only after a Configure whose answer said it would,
	// and only after that answer is on the wire — it cancels the context this
	// connection is served on, so firing it a moment early would leave the
	// operator watching a form that appeared to fail.
	//
	// It exists because both agent backend families latch their toolset at
	// construction: a process that came up with no agent cannot grow one. nil is
	// a node that applies configuration and keeps running without it, which is
	// only useful to a test.
	Restart func()
	// WindowBytes, AckThreshold, AckInterval and Epoch go to the link. Zero
	// takes the link's defaults — which is wrong over a real socket; see
	// nodesocket.DefaultWindowBytes.
	WindowBytes  int
	AckThreshold uint64
	AckInterval  time.Duration
	Epoch        uint64
}

// Server answers one gateway's requests against one agent.
type Server struct {
	client agent.Client
	log    *slog.Logger
	enc    *agentwire.Encoder
	link   *nodelink.Link
	ready  chan struct{}
	proxy  *ToolProxy
	claim  *Advertiser
	// configure applies a configuration the gateway hands this node. nil
	// refuses the method; see Options.Configure.
	configure func(ctx context.Context, cfg agentwire.NodeConfiguration) (agentwire.NodeConfigured, error)
	restart   func()

	ctx    context.Context
	cancel context.CancelFunc

	// nextID numbers the requests this side mints — today only the tool
	// channel's. Ids are per DIRECTION: the gateway's counter also starts at
	// "1", and the two are never compared, which is why calls below is a
	// separate map from anything the gateway's ids index.
	nextID atomic.Int64

	mu    sync.Mutex
	turns map[string]*turn
	asks  map[string]*ask
	calls map[string]chan agentwire.Message
	// headless is the set of session ids opened with no human behind them — a
	// scheduled job, a workflow trigger, an unfurl. A turn on one of these is
	// served WITHOUT a stream on its context, which is what makes the approval
	// gate and every gateway-side interactive tool take their documented
	// no-human branch instead of raising a frame nobody can answer. See
	// servePrompt.
	//
	// It is keyed by session rather than carried on the prompt because the fact
	// belongs to the session: the gateway opens one, prompts it once, and closes
	// it, and a second prompt on the same session is the same delegation.
	headless map[string]bool
}

// turn is one in-flight prompt.
type turn struct {
	sessionID string
	cancel    context.CancelFunc
}

// ask is one outstanding permission request, from either gate.
//
// answer is non-nil for a GateTool request, which this server owns end to end;
// for a GateAgent request the Encoder holds the backend's own channel and the
// answer is delivered through it. Keeping both in one map is what makes "fail
// every approval belonging to this turn" a single loop rather than two
// bookkeeping schemes that drift.
type ask struct {
	stream  string
	answer  chan agentwire.PermissionResponse
	display bool
}

// Serve runs one connection to completion and returns why it ended.
//
// It closes the link when ctx ends but does NOT close the agent client: the
// node outlives any single gateway connection, and a reconnect must find its
// backends where it left them. The sessions themselves do not survive — the
// gateway's session ids are its own — which is #170's stated position that a
// dropped node loses its sessions and the takeover card is the recovery path.
func Serve(ctx context.Context, conn nodelink.Conn, client agent.Client, opts Options) error {
	if client == nil {
		return errors.New("nodeserve: no agent to serve")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		client:   client,
		log:      log,
		enc:      agentwire.NewEncoder(),
		ready:    make(chan struct{}),
		turns:    make(map[string]*turn),
		asks:     make(map[string]*ask),
		calls:    make(map[string]chan agentwire.Message),
		headless: make(map[string]bool),
		// Set at construction, not alongside the binds below: the read loop
		// starts inside nodelink.New and serveInitialize reads this field off
		// that loop, so assigning it afterwards is a data race with the first
		// frame the gateway sends.
		proxy: opts.Tools,
		// Same reason: serveInitialize reads the claim to put on the handshake
		// answer, and that runs off the read loop before the binds below.
		claim: opts.Advertise,
		// And the same again: the gateway may send node.configure at any point
		// after the handshake, which is served off that loop.
		configure: opts.Configure,
		restart:   opts.Restart,
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
	// The read loop starts inside New, so a frame can reach consume before the
	// assignment above lands. Nothing may touch s.link until this closes.
	close(s.ready)

	if opts.Gate != nil {
		opts.Gate.bind(s)
		defer opts.Gate.unbind(s)
	}
	if opts.Background != nil {
		opts.Background.bind(s)
		defer opts.Background.unbind(s)
	}
	if opts.Tools != nil {
		opts.Tools.bind(s)
		defer opts.Tools.unbind(s)
	}
	if opts.Advertise != nil {
		opts.Advertise.bind(s)
		defer opts.Advertise.unbind(s)
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

// consume is the link's handler.
func (s *Server) consume(payload []byte) error {
	<-s.ready
	msg, err := agentwire.DecodeMessage(payload)
	if err != nil {
		// Not a delivery failure: an undecodable frame is a build mismatch, and
		// killing the link over it would take every conversation with it.
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
		// The tool channel's answers. Applied on the read loop because it is a
		// buffered channel send that cannot block — exactly as a permission
		// answer is. Handing this to a goroutine would reorder nothing and cost
		// one per call.
		s.deliverResponse(msg)
	default:
		s.log.Warn("nodeserve: unexpected frame from gateway", "kind", msg.Kind, "id", msg.ID)
	}
	return nil
}

// deliverResponse routes an answer to the call that is waiting for it.
func (s *Server) deliverResponse(msg agentwire.Message) {
	s.mu.Lock()
	waiting := s.calls[msg.ID]
	delete(s.calls, msg.ID)
	s.mu.Unlock()
	if waiting == nil {
		// The caller gave up, or the turn ended under it. Ordinary, not an
		// error: a tool call abandoned mid-flight still gets its answer.
		s.log.Debug("nodeserve: response for a call nobody is waiting on", "id", msg.ID)
		return
	}
	waiting <- msg
	close(waiting)
}

// callGateway performs one node → gateway round trip and returns the answer.
//
// It is called from the goroutine running a tool's Invoke — which is inside the
// agent's own turn, never on the link's read loop. That matters: nodelink acks a
// frame only once its handler returns, so a call issued from the read loop and
// waiting on the gateway's answer would deadlock the link in both directions.
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
		// Both can fire together, and which one the model is told about
		// matters. A turn's context on a node is cancelled BY the link dying —
		// shutdown ends every turn before it fails the calls — so when both are
		// ready the link's verdict is the true one and the more actionable.
		// Go's select picks at random among ready cases, so the priority is
		// spelled out rather than left to chance.
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
			// failCalls closed it: the connection ended while this call was in
			// flight. A closed channel rather than a fault frame, because the
			// answer never came and inventing one that looks like the gateway's
			// would be a lie about where the failure happened.
			return agentwire.Message{}, errLinkGone
		}
		if fault := response.Fault(); fault != nil {
			return agentwire.Message{}, fault
		}
		return response, nil
	}
}

// failCalls ends every outstanding tool call when the connection does.
//
// It is the node-side half of the no-retry abort policy, and the reason it is
// unconditional is that nothing in the tool surface says whether a call may be
// repeated: tools.Tool is Name/Description/InputSchema/Invoke, and neither of
// the two optional interfaces beside it is about retryability. With no
// idempotency information the only sound policy is to fail everything and retry
// nothing. The wording the model sees is written once, in tools.go.
func (s *Server) failCalls() {
	s.mu.Lock()
	waiting := s.calls
	s.calls = make(map[string]chan agentwire.Message)
	s.mu.Unlock()
	for _, ch := range waiting {
		close(ch)
	}
}

// serve answers one request. It runs on its own goroutine; see the package doc.
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
	case agentwire.MethodClose:
		s.reply(msg.ID, agentwire.Empty{})
		s.cancel()
		_ = s.link.Close()
	default:
		s.fault(msg.ID, fmt.Errorf("nodeserve: this node serves no %q request", msg.Method))
	}
}

func (s *Server) serveInitialize(msg agentwire.Message) {
	// The tool surface is fetched HERE, immediately before the agent comes up,
	// and the ordering is load-bearing rather than tidy. Both backend families
	// latch, at different moments: a native agent resolves its toolset on its
	// first Initialize, and an acp/claude_code agent's aggregator resolves it
	// when its first session is registered (agentbuild.acpAggregator's
	// resolvedToolset, which is lazy for exactly this reason). A proxy populated
	// after either point would be a registry that backend never consults again —
	// the same construct-once, latch, warn-only shape that made #185 invisible.
	//
	// A gateway that cannot answer fails the handshake. That is deliberate: the
	// node's redial loop retries in about a second, whereas publishing an agent
	// with an empty toolset would look like working software to everyone except
	// the user asking it to do something.
	if s.proxy != nil {
		if err := s.proxy.refresh(s.ctx, s); err != nil {
			s.fault(msg.ID, fmt.Errorf("nodeserve: could not fetch this gateway's tool surface: %w", err))
			return
		}
	}
	if err := s.client.Initialize(s.ctx); err != nil {
		s.fault(msg.ID, err)
		return
	}
	var result agentwire.InitializeResult
	// Absent means "this node does not know", which the gateway degrades to
	// interruptible. A plain false would be a lie that silently disables
	// interrupting a turn, so a backend with no probe leaves the field unset —
	// exactly as session_manager.go's own assertion behaves in process.
	if prober, ok := s.client.(interface {
		SupportsCancel(context.Context) bool
	}); ok {
		answer := prober.SupportsCancel(s.ctx)
		result.Interruptible = &answer
	}
	// What this node claims rides the handshake answer, and it rides it for an
	// ordering reason rather than for economy. The gateway builds its registry
	// entry the instant this reply lands, so a claim carried here is in hand
	// exactly when there is somewhere to put it; a node.advertise frame sent at
	// the same moment would be racing the gateway's own bookkeeping, and losing
	// that race drops the claim for a window nobody would think to look at.
	// Changes, which have no such problem, are pushed.
	if s.claim != nil {
		result.Advertisement = s.claim.Current()
	}
	s.replyResult(msg.ID, result)
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
	// The stream id rides the turn context so the approval gate, called from
	// deep inside a tool invocation, knows which turn's stream to raise its
	// request on. It is the only correlation available: an Approver is built
	// per agent, not per turn.
	//
	// A HEADLESS turn deliberately gets none, and this omission is the whole
	// mechanism. In process a delegated agent is built with no approver at all,
	// so a job never asks; a node cannot do that, because it builds one gate per
	// agent long before it knows which turns will have a thread. Withholding the
	// stream reaches the same place through the branch that already exists:
	// ToolGate.Approve sees no stream and runs ungated, and the gateway's own
	// side sees a tool call naming no turn, resolves no TurnLocation, and lets
	// `ask`, `present_plan` and the approver take their documented no-thread
	// paths. A job that raised a card instead would block on an answer nobody
	// can give until its timeout burned — the wedged-03:00-job failure.
	if !s.isHeadless(body.SessionID) {
		turnCtx = withStream(turnCtx, msg.ID)
	}

	s.mu.Lock()
	s.turns[msg.ID] = &turn{sessionID: body.SessionID, cancel: cancel}
	s.mu.Unlock()

	events, err := s.client.Prompt(turnCtx, body.SessionID, body.Prompt.Decode())
	if err != nil {
		s.endTurn(msg.ID, false)
		s.fault(msg.ID, err)
		return
	}
	// The acceptance goes out before the first event so a gateway reading its
	// answer synchronously — which both consumers do — is never left rendering
	// a rejection as an empty reply.
	s.reply(msg.ID, agentwire.Empty{})
	go s.pump(turnCtx, msg.ID, events)
}

// isHeadless reports whether this session was opened with nobody behind it.
//
// An unknown session id reads as NOT headless, which is the conservative answer
// in the direction that matters: an unknown session is one this server did not
// open, and treating it as headless would silently ungate a chat turn.
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
	// The backend's own interrupt, not our context: cancelling the turn context
	// here would abandon the event channel the pump is still draining, and both
	// gateway consumers block until that channel closes. The backend closes it.
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
	// Deliberately unanswered: the gateway fires this from a queue with no
	// pending answer registered, and a reply would be logged there as a
	// response to a request nobody made.
	s.mu.Lock()
	delete(s.headless, ref.SessionID)
	s.mu.Unlock()
	if closer, ok := s.client.(interface{ CloseSession(string) }); ok {
		closer.CloseSession(ref.SessionID)
	}
}

// serveConfigure applies the configuration a Slack onboarding form produced for
// this node's owner.
//
// It is answered rather than fire-and-forget because a human is waiting for the
// outcome in Slack: "saved two profiles, restarting" and "this node is already
// configured" are two different things for the gateway to say, and a node that
// silently ignored the frame would leave the operator watching a form that
// appeared to work.
//
// The node decides. A build that wired no applier refuses, and an applier that
// refuses because the node already holds profiles refuses — see
// agentwire.NodeConfiguration for why that guarantee is enforced here and not on
// the gateway.
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
	// After the reply, never before: the restart cancels the context this
	// connection is being served on.
	if result.Restarting && s.restart != nil {
		s.restart()
	}
}

// answer routes a permission answer to whichever gate raised it.
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
		// Already answered, or abandoned when its turn ended. Not an error: a
		// human clicking a card the moment a turn is cancelled is ordinary.
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
	delete(s.asks, answer.ID)
	s.mu.Unlock()
	if pending == nil {
		s.log.Debug("nodeserve: display answer for an unknown request", "id", answer.ID)
		return
	}
	if err := s.enc.Answer(answer); err != nil {
		s.log.Warn("nodeserve: deliver display answer", "error", err, "id", answer.ID)
	}
}

// register records an outstanding permission request against its turn.
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

func (s *Server) forget(id string) {
	s.mu.Lock()
	delete(s.asks, id)
	s.mu.Unlock()
}

// dismissTurnAsks answers every approval still outstanding on a turn with an
// empty decision — the same answer a card that timed out produces in process,
// which both backends already translate into "dismissed without an answer".
//
// It is not optional bookkeeping. claude_code's control request waits on its
// process exiting rather than on the turn's context, so an unanswered approval
// leaves a backend goroutine parked until the agent is killed.
func (s *Server) dismissTurnAsks(stream string) {
	s.mu.Lock()
	dismissals := make([]agentwire.PermissionResponse, 0, len(s.asks))
	var displays []agentwire.DisplayAnswer
	for id, pending := range s.asks {
		if pending.stream != stream {
			continue
		}
		if pending.display {
			displays = append(displays, agentwire.DisplayAnswer{ID: id, Outcome: string(agent.DisplayDismissed)})
			continue
		}
		resp := agentwire.PermissionResponse{ID: id}
		if pending.answer != nil {
			// A tool gate's answer is a (allowed, note) pair and the note is
			// the tool call's result. An empty one would hand the model a
			// blank string as the reason its action did not happen.
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
}

// endTurn releases a turn and, when the stream is still healthy, closes it.
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

// shutdown fails everything still open when the connection ends.
func (s *Server) shutdown() {
	s.mu.Lock()
	streams := make([]string, 0, len(s.turns))
	for id := range s.turns {
		streams = append(streams, id)
	}
	s.mu.Unlock()
	for _, id := range streams {
		// No terminal frame: the link is gone, so there is nothing to send it
		// down. The gateway's own failAll is what tells the user.
		s.endTurn(id, false)
	}
	s.failCalls()
	s.cancel()
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

// sendOn writes a frame under a caller's context, for the paths that must stop
// when their turn does.
func (s *Server) sendOn(ctx context.Context, msg agentwire.Message) error {
	raw, err := msg.Encode()
	if err != nil {
		return err
	}
	return s.link.Send(ctx, raw)
}
