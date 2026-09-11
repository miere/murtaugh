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

var ErrClientClosed = errors.New("remote: client is closed")

const (
	defaultEventBuffer  = 32
	closeQueueDepth     = 64
	closeSessionTimeout = 30 * time.Second
	goodbyeTimeout      = 5 * time.Second
	abandonTimeout      = 5 * time.Second
	answerTimeout       = 30 * time.Second
)

type Options struct {
	Logger    *slog.Logger
	Deliverer agentwire.AttachmentDeliverer
	// A nil Approve denies: defaulting to allow would let an agent whose gateway forgot to wire
	// this run every side-effecting tool unprompted.
	Approve    func(ctx context.Context, toolName, summary string) (allowed bool, note string)
	Background func(sessionID string, ev agent.Event)
	// A nil Advertise drops claims without failing the push: the node cannot know whether the
	// gateway keeps a registry and must not log that as its own fault.
	Advertise Advertiser
	// Owner is the Slack user this node's credential was minted for. Every
	// sign-in the node raises is drawn for them, whatever the node says.
	Owner string

	SignIns func(ctx context.Context, prompt *agent.SignInPrompt, settled <-chan agent.SignInSettled, shown func(error))

	Credentials  func(agentwire.CredentialHealth)
	EventBuffer  int
	WindowBytes  int
	AckThreshold uint64
	AckInterval  time.Duration
	Epoch        uint64
}

type Client struct {
	link       *nodelink.Link
	log        *slog.Logger
	decoder    *agentwire.Decoder
	background func(sessionID string, ev agent.Event)
	approve    func(ctx context.Context, toolName, summary string) (bool, string)
	advertiser Advertiser
	owner      string
	signIn     func(ctx context.Context, prompt *agent.SignInPrompt, settled <-chan agent.SignInSettled, shown func(error))
	transfers  *transfers
	buffer     int

	credentials func(agentwire.CredentialHealth)

	nextID atomic.Int64
	closes chan string

	mu             sync.Mutex
	closed         bool
	pending        map[string]chan agentwire.Message
	streams        map[string]*stream
	answers        map[string]*agentwire.PendingDecision
	signIns        map[string]chan struct{}
	headless       map[string]*headlessSignIn
	interruptible  *bool
	resolved       bool
	advertisedOnce bool
}

// New takes ownership of conn: Close, and any link failure, closes it.
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
		advertiser: opts.Advertise,
		owner:      opts.Owner,
		signIn:     opts.SignIns,
		transfers:  incoming,
		buffer:     buffer,
		closes:     make(chan string, closeQueueDepth),
		pending:    make(map[string]chan agentwire.Message),
		streams:    make(map[string]*stream),
		answers:    make(map[string]*agentwire.PendingDecision),
		signIns:    make(map[string]chan struct{}),
		headless:   make(map[string]*headlessSignIn),

		credentials: opts.Credentials,
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

func (c *Client) Initialize(ctx context.Context) error {
	var result agentwire.InitializeResult
	if err := c.call(ctx, agentwire.MethodInitialize, agentwire.Empty{}, &result); err != nil {
		return err
	}
	c.mu.Lock()
	c.interruptible = result.Interruptible
	c.resolved = true
	c.mu.Unlock()
	c.applyAdvertisement(result.Advertisement, true)
	if result.Interruptible == nil {
		c.log.Warn("node did not report whether its agent can be interrupted; assuming it can")
		return nil
	}
	c.log.Info("initialized remote agent", "interruptible", *result.Interruptible)
	return nil
}

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

// Prompt returns the node's rejection synchronously because both consumers check it before
// rendering and would otherwise show a failure as an empty reply.
func (c *Client) Prompt(ctx context.Context, sessionID string, request agent.PromptRequest) (<-chan agent.Event, error) {
	id := c.mintID()
	s := newStream(sessionID, c.buffer)
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

func (c *Client) watchTurn(ctx context.Context, id string, s *stream) {
	select {
	case <-s.quit:
	case <-c.link.Done():
	case <-ctx.Done():
		c.abandon(s.sessionID)
		c.dropStream(id)
	}
}

// Cancel can outlive ctx's deadline by up to one link write timeout, because Link.Send takes
// an uncancellable mutex before it checks the context.
func (c *Client) Cancel(ctx context.Context, sessionID string) error {
	return c.call(ctx, agentwire.MethodCancel, agentwire.SessionRef{SessionID: sessionID}, nil)
}

// Configure is not on agent.Client because only a caller that knows it is talking to a node
// can use it. A refusal is normal: a node only accepts configuration while it has none.
func (c *Client) Configure(ctx context.Context, cfg agentwire.NodeConfiguration) (agentwire.NodeConfigured, error) {
	var out agentwire.NodeConfigured
	if err := c.call(ctx, agentwire.MethodConfigure, cfg, &out); err != nil {
		return agentwire.NodeConfigured{}, err
	}
	return out, nil
}

// RenewCredential is not on agent.Client for the reason Configure is not: only
// a caller that knows it is talking to a node can ask a node to sign in.
func (c *Client) RenewCredential(ctx context.Context) (agentwire.CredentialRenewal, error) {
	var out agentwire.CredentialRenewal
	if err := c.call(ctx, agentwire.MethodRenewCredential, agentwire.Empty{}, &out); err != nil {
		return agentwire.CredentialRenewal{}, err
	}
	return out, nil
}

// CloseSession never waits on the network because SessionManager calls it while holding its
// mutex. Skipping it would leak one node-side process per evicted conversation.
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

// SupportsCancel treats a node that never answered as interruptible, so a missing answer can
// never silently stop interrupts from working.
func (c *Client) SupportsCancel(context.Context) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.resolved || c.interruptible == nil {
		return true
	}
	return *c.interruptible
}

// A client whose link has died never recovers, because a node that disappeared has to dial
// back in.
func (c *Client) Done() <-chan struct{} { return c.link.Done() }

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
	c.transfers.close()
	return err
}

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
		go c.serveRequest(msg)
	case agentwire.MessageChunk:
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
		c.dismissOrphanDisplay(msg)
		return
	}
	if len(msg.Body) > 0 {
		ev, handled, err := c.decode(msg, s)
		switch {
		case err != nil:
			s.send(agent.Event{Type: agent.EventError, Error: err})
		case handled:
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
	ev, handled, err := c.decode(msg, nil)
	if handled {
		return
	}
	if err != nil {
		ev = agent.Event{Type: agent.EventError, Error: err}
	}
	c.background(msg.SessionID, ev)
}

func (c *Client) dismissOrphanDisplay(msg agentwire.Message) {
	var wire agentwire.Event
	if len(msg.Body) == 0 || msg.Into(&wire) != nil {
		return
	}
	id := ""
	switch {
	case wire.Question != nil:
		id = wire.Question.ID
	case wire.Plan != nil:
		id = wire.Plan.ID
	case wire.SignIn != nil:
		id = wire.SignIn.ID
	default:
		return
	}
	go c.sendDisplayAnswer(agentwire.DisplayAnswer{ID: id, Outcome: string(agent.DisplayDismissed)})
}

func (c *Client) decode(msg agentwire.Message, s *stream) (agent.Event, bool, error) {
	var wire agentwire.Event
	if err := msg.Into(&wire); err != nil {
		return agent.Event{}, false, err
	}
	if settled := wire.SignInSettled; settled != nil && (s == nil || !c.decoder.SignInOpen(settled.ID)) {
		c.log.Debug("remote: a sign-in settled that nothing here is drawing", "id", settled.ID, "state", settled.State)
		return agent.Event{}, true, nil
	}
	ev, pending, err := c.decoder.Decode(context.Background(), wire)
	if err != nil {
		return agent.Event{}, false, err
	}
	if ev.SignInSettled != nil && ev.SignInSettled.State.Terminal() {
		c.settleSignIn(wire.SignInSettled.ID)
	}
	if pending == nil {
		return ev, false, nil
	}
	if ev.SignIn != nil {
		if c.owner == "" {
			c.decoder.ForgetSignIn(pending.ID)
			c.log.Warn("remote: refusing a sign-in from a node whose token names no owner", "id", pending.ID)
			go c.sendDisplayAnswer(agentwire.DisplayAnswer{ID: pending.ID, Outcome: string(agent.DisplayUnavailable),
				Note: "this machine's token names no owner, so nobody can be asked to sign in"})
			return agent.Event{}, true, nil
		}
		if s == nil || s.location.ChannelID == "" {
			c.decoder.ForgetSignIn(pending.ID)
			go c.sendDisplayAnswer(agentwire.DisplayAnswer{ID: pending.ID, Outcome: string(agent.DisplayNoConversation)})
			return agent.Event{}, true, nil
		}
		ev.SignIn.Owner = c.owner
		settled := make(chan struct{})
		c.mu.Lock()
		c.signIns[pending.ID] = settled
		c.mu.Unlock()
		go c.relaySignIn(pending.ID, ev.SignIn, s, settled)
		return ev, false, nil
	}
	if answers := displayAnswers(ev); answers != nil {
		if s == nil || s.location.ChannelID == "" {
			go c.sendDisplayAnswer(agentwire.DisplayAnswer{ID: pending.ID, Outcome: string(agent.DisplayNoConversation)})
			return agent.Event{}, true, nil
		}
		go c.answerDisplay(pending.ID, answers, s)
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

func (c *Client) answerApproval(pending *agentwire.PendingDecision, ev agent.Event, s *stream) {
	toolName, summary := "", ""
	if ev.Permission != nil {
		toolName, summary = ev.Permission.Request.ToolKind, ev.Permission.Request.ToolTitle
	}
	allowed, note := false, "Skipped: this gateway has no approval gate for the agent, so nobody could be asked. The action was not run."
	if c.approve != nil {
		ctx := context.Background()
		if s != nil {
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

func displayAnswers(ev agent.Event) chan agent.DisplayAnswer {
	switch {
	case ev.Question != nil:
		return ev.Question.Answer
	case ev.Plan != nil:
		return ev.Plan.Answer
	}
	return nil
}

func (c *Client) answerDisplay(id string, answers <-chan agent.DisplayAnswer, s *stream) {
	select {
	case answer := <-answers:
		c.sendDisplayAnswer(agentwire.EncodeDisplayAnswer(id, answer))
	case <-s.quit:
		select {
		case answer := <-answers:
			c.sendDisplayAnswer(agentwire.EncodeDisplayAnswer(id, answer))
		default:
			c.sendDisplayAnswer(agentwire.DisplayAnswer{ID: id, Outcome: string(agent.DisplayDismissed)})
		}
	case <-c.link.Done():
	}
}

func (c *Client) relaySignIn(id string, prompt *agent.SignInPrompt, s *stream, settled <-chan struct{}) {
	defer c.dropSignIn(id)
	for {
		select {
		case answer := <-prompt.Answer:
			c.sendDisplayAnswer(agentwire.EncodeDisplayAnswer(id, answer))
			if answer.Outcome != agent.DisplayAnswered && answer.Outcome != agent.DisplayApproved {
				return
			}
		case <-settled:
			return
		case <-s.quit:
			select {
			case <-settled:
			default:
				c.sendDisplayAnswer(agentwire.DisplayAnswer{ID: id, Outcome: string(agent.DisplayDismissed)})
			}
			return
		case <-c.link.Done():
			return
		}
	}
}

func (c *Client) settleSignIn(id string) {
	c.mu.Lock()
	settled := c.signIns[id]
	delete(c.signIns, id)
	c.mu.Unlock()
	if settled != nil {
		close(settled)
	}
}

func (c *Client) dropSignIn(id string) {
	c.mu.Lock()
	delete(c.signIns, id)
	c.mu.Unlock()
	c.decoder.ForgetSignIn(id)
}

func (c *Client) sendDisplayAnswer(answer agentwire.DisplayAnswer) {
	msg, err := agentwire.AnswerDisplay(answer)
	if err != nil {
		c.log.Warn("remote: encode display answer", "error", err, "id", answer.ID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), abandonTimeout)
	defer cancel()
	if err := c.send(ctx, msg); err != nil && !errors.Is(err, nodelink.ErrLinkClosed) {
		c.log.Warn("remote: deliver display answer", "error", err, "id", answer.ID)
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

func (c *Client) watchLink() {
	<-c.link.Done()
	c.failAll(c.link.Err())
	c.transfers.close()
}

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

func (c *Client) mintID() string {
	return strconv.FormatInt(c.nextID.Add(1), 10)
}

var (
	_ agent.Client                                      = (*Client)(nil)
	_ interface{ CloseSession(string) }                 = (*Client)(nil)
	_ interface{ SupportsCancel(context.Context) bool } = (*Client)(nil)
)
