package remote

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/nodelink"
)

// ErrClientClosed is returned by a call made after Close.
var ErrClientClosed = errors.New("remote: client is closed")

const (
	// defaultEventBuffer matches the ACP client's per-turn channel depth, so a
	// remote turn absorbs the same burst before it starts applying
	// backpressure as a local one.
	defaultEventBuffer = 32
	// closeQueueDepth bounds the CloseSession queue. CloseSession runs under
	// SessionManager.mu and cannot wait, so the queue is what makes it return
	// immediately; a full queue is reported rather than silently dropped,
	// because each lost entry is a node-side session process nobody will
	// reclaim.
	closeQueueDepth = 64
	// closeSessionTimeout bounds the enqueued send. It is generous: the frame
	// is small and losing it leaks a process.
	closeSessionTimeout = 30 * time.Second
	// goodbyeTimeout bounds the close handshake. The link is torn down
	// regardless; this only gives the node the chance to shut down cleanly.
	goodbyeTimeout = 5 * time.Second
	// abandonTimeout bounds the cancel sent when a caller's context is
	// cancelled. It must be short: it runs on a teardown path.
	abandonTimeout = 5 * time.Second
	// toolAnswerTimeout bounds delivery of a tool call's answer back to the
	// node. It is short because by the time it expires the node has already
	// given up on this call — nodeserve fails an in-flight call the moment its
	// link drops — so a longer wait only holds a goroutine.
	toolAnswerTimeout = 30 * time.Second
)

// Options configures a Client.
type Options struct {
	Logger *slog.Logger
	// Deliverer materialises a side-transferred attachment. nil takes this
	// package's own collector, which buffers the chunks that arrive ahead of
	// the event into a file; supply one only to override that.
	Deliverer agentwire.AttachmentDeliverer
	// Approve answers a GateTool permission request — the native loop's inline
	// approval, which in process is a direct call to the gateway's approver and
	// has no event of its own. It returns the pair that call returns: whether to
	// run the tool, and the note handed BACK TO THE MODEL as the call's result
	// when not.
	//
	// nil denies every such request with a note saying nobody could be asked.
	// Defaulting to allow would mean an agent whose gateway forgot to wire this
	// runs every side-effecting tool unprompted.
	Approve func(ctx context.Context, toolName, summary string) (allowed bool, note string)
	// Background receives events that belong to no request: a background turn
	// completing against a session the gateway is not prompting. nil logs and
	// drops them, which is what the gateway does today for an agent with no
	// router bound.
	Background func(sessionID string, ev agent.Event)
	// Tools answers the node's tool.list and tool.call requests — Murtaugh's own
	// tools, executed HERE with the gateway's credentials, subject to the
	// partition the implementation applies.
	//
	// nil refuses both with a legible fault rather than answering an empty list.
	// An empty list would be indistinguishable from "you may reach nothing",
	// which is a real answer, and a node that took it would publish an agent
	// with a silently empty toolset — the exact regression #194 exists to
	// prevent.
	Tools ToolHost
	// Advertise receives what this node claims to serve: at the handshake, from
	// inside Initialize, and again on every node-side configuration change.
	//
	// nil drops the claim, which is what a gateway with no registry does. It
	// is not an error: the node cannot know whether the gateway keeps one, and
	// failing its push would make the node's own logs blame it for the
	// gateway's shape.
	Advertise Advertiser
	// EventBuffer is a turn's channel depth. Zero takes defaultEventBuffer.
	EventBuffer int
	// WindowBytes, AckThreshold, AckInterval and Epoch are passed through to
	// the link. Zero takes the link's defaults.
	WindowBytes  int
	AckThreshold uint64
	AckInterval  time.Duration
	Epoch        uint64
}

// Client is agent.Client talking to a runtime node over one link.
type Client struct {
	link    *nodelink.Link
	log     *slog.Logger
	decoder *agentwire.Decoder
	// background receives events addressed to a session rather than to a
	// request: the path a background turn's completions arrive on.
	background func(sessionID string, ev agent.Event)
	approve    func(ctx context.Context, toolName, summary string) (bool, string)
	tools      ToolHost
	advertiser Advertiser
	transfers  *transfers
	buffer     int

	nextID atomic.Int64
	closes chan string

	mu     sync.Mutex
	closed bool
	// pending holds answers to requests THIS side minted; inbound holds the
	// cancel funcs of requests the NODE minted. Two maps, because ids are per
	// direction and both counters start at "1".
	pending       map[string]chan agentwire.Message
	inbound       map[string]context.CancelFunc
	streams       map[string]*stream
	answers       map[string]*agentwire.PendingDecision
	interruptible *bool
	resolved      bool
	// advertisedOnce is set by the first claim to land, whichever path it came
	// in on. See applyAdvertisement: it is the whole of the ordering rule.
	advertisedOnce bool
}

// New builds a client over conn and starts reading. conn belongs to the client
// from here: Close, and any link failure, closes it.
func New(conn nodelink.Conn, opts Options) *Client {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	buffer := opts.EventBuffer
	if buffer <= 0 {
		buffer = defaultEventBuffer
	}
	incoming := newTransfers(log)
	deliverer := opts.Deliverer
	if deliverer == nil {
		deliverer = incoming
	}
	c := &Client{
		log:        log,
		decoder:    agentwire.NewDecoder(deliverer),
		background: opts.Background,
		approve:    opts.Approve,
		tools:      opts.Tools,
		advertiser: opts.Advertise,
		transfers:  incoming,
		buffer:     buffer,
		closes:     make(chan string, closeQueueDepth),
		pending:    make(map[string]chan agentwire.Message),
		inbound:    make(map[string]context.CancelFunc),
		streams:    make(map[string]*stream),
		answers:    make(map[string]*agentwire.PendingDecision),
	}
	c.link = nodelink.New(conn, nodelink.Options{
		Handler:      c.consume,
		Logger:       log,
		WindowBytes:  opts.WindowBytes,
		AckThreshold: opts.AckThreshold,
		AckInterval:  opts.AckInterval,
		Epoch:        opts.Epoch,
	})
	go c.closeLoop()
	go c.watchLink()
	return c
}

// Initialize brings the node's agent up and resolves its capabilities once.
func (c *Client) Initialize(ctx context.Context) error {
	var result agentwire.InitializeResult
	if err := c.call(ctx, agentwire.MethodInitialize, agentwire.Empty{}, &result); err != nil {
		return err
	}
	c.mu.Lock()
	c.interruptible = result.Interruptible
	c.resolved = true
	c.mu.Unlock()
	// Delivered here, synchronously, before Initialize returns: the caller
	// publishes the node the instant this succeeds, so the claim has to be in
	// hand by then or the node is briefly attached and mute. A node pushing its
	// opening claim as a node.advertise request instead would be racing exactly
	// that window — see agentwire.InitializeResult.
	c.applyAdvertisement(result.Advertisement, true)
	if result.Interruptible == nil {
		// Visible degradation. The verdict defaults the way the session
		// manager's own unresolved probe does, and it is not left to be
		// inferred from an absent log line.
		c.log.Warn("node did not report whether its agent can be interrupted; assuming it can")
		return nil
	}
	c.log.Info("initialized remote agent", "interruptible", *result.Interruptible)
	return nil
}

// NewSession opens a conversation on the node and returns the id every later
// call names.
func (c *Client) NewSession(ctx context.Context, meta agent.SessionMetadata) (agent.Session, error) {
	var result agentwire.NewSessionResult
	if err := c.call(ctx, agentwire.MethodNewSession, agentwire.EncodeSessionMetadata(meta), &result); err != nil {
		return agent.Session{}, err
	}
	if result.SessionID == "" {
		return agent.Session{}, fmt.Errorf("remote: node created a session with no id")
	}
	return agent.Session{ID: result.SessionID}, nil
}

// Prompt runs one turn and returns its event stream.
//
// The stream is registered BEFORE the request goes out, because the node is
// free to start emitting events the moment it accepts the prompt and an event
// that arrives before the acceptance still belongs to the turn.
//
// The returned error is the node's rejection, read synchronously — which is the
// contract both consumers depend on, since they check it before any rendering
// starts and would otherwise render a failure as an empty reply.
func (c *Client) Prompt(ctx context.Context, sessionID string, request agent.PromptRequest) (<-chan agent.Event, error) {
	id := c.mintID()
	s := newStream(sessionID, c.buffer)
	// Only when there is somewhere to point at, which leaves a headless turn — a
	// job, an unfurl, a workflow trigger — holding the ZERO location. That is
	// the honest answer and it is the one every consumer is written for:
	// agent.TurnLocationFromContext returns `ok && loc.ChannelID != ""`, so a
	// zero location reads as absent to interaction.GateApprover, `ask` and
	// `present_plan` alike, and each takes its no-thread branch. It is also what
	// the in-process native client does — it stamps the location unconditionally
	// — so both paths present a headless turn identically.
	if strings.TrimSpace(request.Channel) != "" {
		s.location = agent.TurnLocation{ChannelID: request.Channel, ThreadTS: request.Thread, UserID: request.User}
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClientClosed
	}
	c.streams[id] = s
	c.mu.Unlock()

	body := agentwire.PromptBody{SessionID: sessionID, Prompt: agentwire.EncodePromptRequest(request)}
	if err := c.callWithID(ctx, id, agentwire.MethodPrompt, body, nil); err != nil {
		c.dropStream(id)
		return nil, err
	}
	go c.watchTurn(ctx, id, s)
	return s.events, nil
}

// watchTurn closes the turn's channel promptly when the caller gives up or the
// link dies, and tells the node to abandon a turn nobody is listening to.
func (c *Client) watchTurn(ctx context.Context, id string, s *stream) {
	select {
	case <-s.quit:
		// The turn ended on its own; the node already knows.
	case <-c.link.Done():
		// failAll owns this teardown. Reporting the failure here as well would
		// put two error events on one turn.
	case <-ctx.Done():
		// In process this context reaches the backend directly. Across a link
		// it reaches nothing, so the turn has to be cancelled explicitly or the
		// node runs it to completion for a consumer that has gone.
		c.abandon(s.sessionID)
		c.dropStream(id)
	}
}

// Cancel interrupts the session's in-flight turn.
//
// It honours ctx's deadline for its own wait, but it does NOT guarantee to
// return within it. Link.Send takes an uncancellable mutex before it consults
// the context, so a sender already wedged inside a transport write holds this
// one behind it; the bound is that wedge's write deadline
// (nodesocket.DefaultWriteTimeout, after which the link is torn down and every
// call fails at once) plus ctx's own. The gateway's idle path calls this under
// five seconds while holding a wedged turn open and gets its answer in that
// long in the ordinary case — but the worst case is one write timeout, not five
// seconds, and item 3 owns the mutex ordering that would make it exact.
//
// An unknown session is the node's business, and answers success there for the
// same reason acp.Client.Cancel returns nil — there is nothing to interrupt and
// nothing to report.
func (c *Client) Cancel(ctx context.Context, sessionID string) error {
	return c.call(ctx, agentwire.MethodCancel, agentwire.SessionRef{SessionID: sessionID}, nil)
}

// Configure hands a node the agent profiles a Slack onboarding form produced
// for its owner, and reports what the node did with them.
//
// It is deliberately NOT on agent.Client. The interface is what a gateway uses
// to run a turn, and every implementation of it — native, acp, claude_code, and
// this one — would have to grow a method that only means anything across a
// network. This is a remote-only capability, reached by the one caller that
// knows it is talking to a node.
//
// The node may refuse, and a refusal is the ordinary case rather than an error
// in the transport: a node applies configuration only while it holds none of its
// own. See agentwire.NodeConfiguration.
func (c *Client) Configure(ctx context.Context, cfg agentwire.NodeConfiguration) (agentwire.NodeConfigured, error) {
	var out agentwire.NodeConfigured
	if err := c.call(ctx, agentwire.MethodConfigure, cfg, &out); err != nil {
		return agentwire.NodeConfigured{}, err
	}
	return out, nil
}

// CloseSession releases one conversation's node-side resources.
//
// It returns immediately, always. The session manager calls it while holding
// its own mutex, with no context and no error return, from Discard and from
// eviction — so a synchronous round trip here would park every conversation on
// this agent behind one network call. The frame is handed to the sender
// goroutine, which delivers it under the link's ordinary guarantees.
//
// It is answered rather than skipped because the two backends that implement it
// each own a real per-conversation process: falling through to the manager's
// no-op would leak one node-side process for every evicted conversation.
func (c *Client) CloseSession(sessionID string) {
	if sessionID == "" {
		return
	}
	select {
	case c.closes <- sessionID:
	default:
		c.log.Warn("remote: session close queue is full; the node may be left holding a session", "session_id", sessionID)
	}
}

// SupportsCancel reports the interruptibility the node declared at initialize.
//
// Unresolved reads as interruptible, matching acp.Client's answer before its
// own probe has run and the session manager's default when no signal is
// available: a missing answer must never be the one that silently stops
// interrupts from working.
func (c *Client) SupportsCancel(context.Context) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.resolved || c.interruptible == nil {
		return true
	}
	return *c.interruptible
}

// StreamLocation returns where in Slack the turn identified by streamID is
// happening, if it is still open.
//
// It is exported for the tool channel: a node's tool call names its turn, and
// the gateway has to put the location back on the context before invoking or
// the approval gate short-circuits to allowed and `ask`/`present_plan` degrade
// to non-interactive. The client already holds this per stream because
// answerApproval needed the same thing.
//
// ok means the STREAM IS KNOWN, and not "this turn has a thread". Those are two
// different questions and conflating them made a headless turn — which has no
// thread and is not supposed to — indistinguishable from a tool call naming a
// turn this gateway has never heard of, which is a real anomaly and is logged as
// one. The location of a known headless turn is the zero value, and
// agent.TurnLocationFromContext reads that as absent, so the caller can stamp it
// unconditionally and every consumer still takes its no-thread branch.
func (c *Client) StreamLocation(streamID string) (agent.TurnLocation, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.streams[streamID]
	if s == nil {
		return agent.TurnLocation{}, false
	}
	return s.location, true
}

// Done closes when the link under this client stops, for any reason. It is what
// the owner of the connection waits on: a client whose link has died answers
// every call with the link's error and never recovers, because a node that
// disappeared has to dial back in.
func (c *Client) Done() <-chan struct{} { return c.link.Done() }

// Close says goodbye, tears the link down and finishes every open turn.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), goodbyeTimeout)
	if err := c.call(ctx, agentwire.MethodClose, agentwire.Empty{}, nil); err != nil && !errors.Is(err, nodelink.ErrLinkClosed) {
		c.log.Warn("remote: node did not acknowledge close", "error", err)
	}
	cancel()

	err := c.link.Close()
	c.failAll(nil)
	c.cancelInbound()
	c.transfers.close()
	return err
}

// consume is the link's handler: one delivered payload, decoded and routed.
//
// It runs on the read loop, so the frame is acknowledged only once this
// returns — which is the point. A payload that cannot be decoded is NOT a
// delivery failure and does not kill the link: it is surfaced on the turn it
// belongs to, where a human sees it.
func (c *Client) consume(payload []byte) error {
	msg, err := agentwire.DecodeMessage(payload)
	if err != nil {
		c.log.Warn("remote: undecodable payload from node", "error", err)
		return nil
	}
	switch msg.Kind {
	case agentwire.MessageResponse:
		c.deliverResponse(msg)
	case agentwire.MessageEvent:
		c.deliverEvent(msg)
	case agentwire.MessageRequest:
		// The tool channel, and an explicit refusal for anything else. It is
		// dispatched off the read loop because a tool call can park on a human
		// for ten minutes and a frame is acknowledged only once this handler
		// returns — see serveRequest.
		go c.serveRequest(msg)
	case agentwire.MessageChunk:
		// Written here, on the read loop, on purpose: the chunks arrive ahead of
		// the event that references them, and this is what makes the deliverer a
		// lookup instead of a pull that would deadlock the loop it runs on.
		c.transfers.accept(msg)
	case agentwire.MessagePermission:
		c.log.Warn("remote: permission answer arrived from the node, which does not answer them", "id", msg.ID)
	default:
		c.log.Warn("remote: unknown message kind from node", "kind", msg.Kind)
	}
	return nil
}

func (c *Client) deliverResponse(msg agentwire.Message) {
	c.mu.Lock()
	ch := c.pending[msg.ID]
	delete(c.pending, msg.ID)
	c.mu.Unlock()
	if ch == nil {
		c.log.Warn("remote: response for an unknown request", "id", msg.ID)
		return
	}
	ch <- msg
	close(ch)
}

func (c *Client) deliverEvent(msg agentwire.Message) {
	if msg.ID == "" {
		c.deliverBackground(msg)
		return
	}
	c.mu.Lock()
	s := c.streams[msg.ID]
	c.mu.Unlock()
	if s == nil {
		// A trailing event for a turn already torn down. Not an error: the
		// consumer walked away and the node had frames in flight.
		return
	}
	// End may ride along with a final event, so the payload is delivered before
	// the channel closes rather than instead of it.
	if len(msg.Body) > 0 {
		ev, handled, err := c.decode(msg, s)
		switch {
		case err != nil:
			s.send(agent.Event{Type: agent.EventError, Error: err})
		case handled:
			// A native tool approval: it is a request the gateway answers, not
			// an event the renderer draws. In process the same call never
			// reaches the event stream either, so putting it there would give a
			// remote native agent a card the local one does not have.
		default:
			s.send(ev)
		}
	}
	if msg.End {
		c.dropStream(msg.ID)
	}
}

func (c *Client) deliverBackground(msg agentwire.Message) {
	if msg.SessionID == "" {
		c.log.Warn("remote: event frame addressed to neither a turn nor a session")
		return
	}
	if c.background == nil {
		c.log.Warn("remote: background event with no router bound", "session_id", msg.SessionID)
		return
	}
	ev, _, err := c.decode(msg, nil)
	if err != nil {
		ev = agent.Event{Type: agent.EventError, Error: err}
	}
	c.background(msg.SessionID, ev)
}

// decode turns an event frame into the agent event the renderer consumes, and
// starts the watcher that carries a permission answer back.
//
// The second result says the frame was a request rather than an event and has
// been taken care of: a GateTool approval is answered by the gateway's tool
// gate directly, off this goroutine, and never reaches the turn's stream.
func (c *Client) decode(msg agentwire.Message, s *stream) (agent.Event, bool, error) {
	var wire agentwire.Event
	if err := msg.Into(&wire); err != nil {
		return agent.Event{}, false, err
	}
	ev, pending, err := c.decoder.Decode(context.Background(), wire)
	if err != nil {
		return agent.Event{}, false, err
	}
	if pending == nil {
		return ev, false, nil
	}
	if pending.Gate == agentwire.GateTool {
		go c.answerApproval(pending, ev, s)
		return agent.Event{}, true, nil
	}
	c.mu.Lock()
	c.answers[pending.ID] = pending
	c.mu.Unlock()
	go c.answerPermission(pending)
	return ev, false, nil
}

// answerApproval runs the gateway's inline tool gate for a native agent on a
// node and sends back the pair it returns.
//
// It runs off the read loop because the gate blocks on a human for up to ten
// minutes, and the read loop is the only thing that can deliver the cancel that
// would end that wait.
func (c *Client) answerApproval(pending *agentwire.PendingDecision, ev agent.Event, s *stream) {
	toolName, summary := "", ""
	if ev.Permission != nil {
		toolName, summary = ev.Permission.Request.ToolKind, ev.Permission.Request.ToolTitle
	}
	allowed, note := false, "Skipped: this gateway has no approval gate for the agent, so nobody could be asked. The action was not run."
	if c.approve != nil {
		ctx := context.Background()
		if s != nil {
			// Without the turn's location the gate short-circuits to "allowed"
			// — its documented headless behaviour — which over a link would
			// silently ungate every tool call on the node. A headless turn's
			// location is the zero value and reads as absent, so it takes
			// exactly that branch; it should also never get this far, because a
			// headless session is served with no stream on its context and its
			// gate never raises a frame.
			ctx = agent.WithTurnLocation(ctx, s.location)
		}
		allowed, note = c.approve(ctx, toolName, summary)
	}
	decision := agent.PermissionDeny
	if allowed {
		decision = agent.PermissionAllow
	}
	msg, err := agentwire.PermissionAnswer(agentwire.PermissionResponse{ID: pending.ID, OptionID: decision, Note: note})
	if err != nil {
		c.log.Warn("remote: encode tool approval", "error", err, "id", pending.ID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), abandonTimeout)
	defer cancel()
	if err := c.send(ctx, msg); err != nil {
		c.log.Warn("remote: deliver tool approval", "error", err, "id", pending.ID)
	}
}

// answerPermission waits for the human's decision and sends it back under the
// id the node minted. The decoded prompt's channel is buffered and written
// exactly once, so this goroutine ends on the first of an answer or the link
// dying.
func (c *Client) answerPermission(pending *agentwire.PendingDecision) {
	defer func() {
		c.mu.Lock()
		delete(c.answers, pending.ID)
		c.mu.Unlock()
	}()
	var optionID string
	select {
	case optionID = <-pending.Decision:
	case <-c.link.Done():
		return
	}
	msg, err := agentwire.PermissionAnswer(agentwire.Response(pending.ID, optionID))
	if err != nil {
		c.log.Warn("remote: encode permission answer", "error", err, "id", pending.ID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), abandonTimeout)
	defer cancel()
	if err := c.send(ctx, msg); err != nil {
		c.log.Warn("remote: deliver permission answer", "error", err, "id", pending.ID)
	}
}

func (c *Client) rejectRequest(msg agentwire.Message) {
	ctx, cancel := context.WithTimeout(context.Background(), abandonTimeout)
	defer cancel()
	fault := agentwire.Fault(msg.ID, fmt.Errorf("remote: the gateway serves no %q request", msg.Method))
	if err := c.send(ctx, fault); err != nil {
		c.log.Warn("remote: reject node request", "error", err, "method", msg.Method)
	}
}

// call mints a request id and performs one round trip.
func (c *Client) call(ctx context.Context, method agentwire.Method, body, out any) error {
	return c.callWithID(ctx, c.mintID(), method, body, out)
}

func (c *Client) callWithID(ctx context.Context, id string, method agentwire.Method, body, out any) error {
	answer := make(chan agentwire.Message, 1)
	c.mu.Lock()
	if c.closed && method != agentwire.MethodClose {
		c.mu.Unlock()
		return ErrClientClosed
	}
	c.pending[id] = answer
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	msg, err := agentwire.Request(id, method, body)
	if err != nil {
		return err
	}
	if err := c.send(ctx, msg); err != nil {
		return fmt.Errorf("remote: send %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.link.Done():
		return c.link.Err()
	case response := <-answer:
		if fault := response.Fault(); fault != nil {
			return fault
		}
		if out == nil {
			return nil
		}
		return response.Into(out)
	}
}

func (c *Client) send(ctx context.Context, msg agentwire.Message) error {
	raw, err := msg.Encode()
	if err != nil {
		return err
	}
	return c.link.Send(ctx, raw)
}

// closeLoop drains the CloseSession queue. It is the goroutine that lets
// CloseSession return under the session manager's mutex.
func (c *Client) closeLoop() {
	for {
		select {
		case <-c.link.Done():
			return
		case sessionID := <-c.closes:
			ctx, cancel := context.WithTimeout(context.Background(), closeSessionTimeout)
			msg, err := agentwire.Request(c.mintID(), agentwire.MethodCloseSession, agentwire.SessionRef{SessionID: sessionID})
			if err == nil {
				err = c.send(ctx, msg)
			}
			cancel()
			if err != nil && !errors.Is(err, nodelink.ErrLinkClosed) {
				c.log.Warn("remote: could not release the node's session", "error", err, "session_id", sessionID)
			}
		}
	}
}

// watchLink fails every open turn when the connection dies, so a node that
// disappears mid-sentence produces a visible failure rather than a channel
// nobody closes.
func (c *Client) watchLink() {
	<-c.link.Done()
	c.failAll(c.link.Err())
	// The tool calls this gateway was running FOR the node are cancelled here
	// too. The node has already failed them for its model, so anything still
	// executing is executing for nobody — and one of them may be parked on an
	// approval card with a ten-minute timeout.
	c.cancelInbound()
	c.transfers.close()
}

// abandon tells the node to stop a turn whose consumer has gone.
func (c *Client) abandon(sessionID string) {
	if sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), abandonTimeout)
	defer cancel()
	if err := c.Cancel(ctx, sessionID); err != nil && !errors.Is(err, nodelink.ErrLinkClosed) && !errors.Is(err, context.Canceled) {
		c.log.Warn("remote: could not abandon the node's turn", "error", err, "session_id", sessionID)
	}
}

// failAll finishes every open turn. err is reported on each of them unless this
// client is the one closing: a deliberate teardown is not a failure to show the
// user, but a link that died under us is.
func (c *Client) failAll(err error) {
	c.mu.Lock()
	closing := c.closed
	streams := c.streams
	c.streams = make(map[string]*stream)
	c.mu.Unlock()
	report := err != nil && !closing
	for _, s := range streams {
		if report {
			s.send(agent.Event{Type: agent.EventError, Error: fmt.Errorf("remote: connection to the node failed: %w", err)})
		}
		s.finish()
	}
}

func (c *Client) dropStream(id string) {
	c.mu.Lock()
	s := c.streams[id]
	delete(c.streams, id)
	c.mu.Unlock()
	if s != nil {
		s.finish()
	}
}

// mintID numbers requests per direction, monotonically and without reuse — the
// same rule the ACP transport's counter follows. Ids are only ever matched
// within the direction that minted them.
func (c *Client) mintID() string {
	return strconv.FormatInt(c.nextID.Add(1), 10)
}

// Compile-time proof that this is the seam it claims to be: agent.Client plus
// both optional surfaces the session manager asserts for on the CLIENT.
var (
	_ agent.Client                                      = (*Client)(nil)
	_ interface{ CloseSession(string) }                 = (*Client)(nil)
	_ interface{ SupportsCancel(context.Context) bool } = (*Client)(nil)
)
