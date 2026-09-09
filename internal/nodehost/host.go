package nodehost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agent/remote"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/journal"
	"github.com/miere/murtaugh/internal/nodesocket"
	"github.com/miere/murtaugh/internal/nodetoken"
	"github.com/miere/murtaugh/internal/tools"
)

// ErrNoNode is what every call made while no node is attached returns. It is a
// sentinel because it is a state a user must be told about in plain words —
// "nothing is connected that can run this" — rather than a transport error.
var ErrNoNode = errors.New("no runtime node is connected")

const (
	// initializeTimeout bounds the handshake's agent initialize. A node that
	// cannot bring its agent up in this long is not one to publish.
	initializeTimeout = 30 * time.Second
	// shutdownGrace bounds the listener's own shutdown.
	shutdownGrace = 5 * time.Second
)

// Options configures a Host.
type Options struct {
	// Tokens is the credential store. Required: a host with no store cannot
	// authenticate anything and must not accept.
	Tokens config.NodeTokenStore
	Logger *slog.Logger
	// Now is the clock the credential's expiry is judged against.
	Now func() time.Time
	// WriteTimeout bounds one transport write. Zero takes the transport's.
	WriteTimeout time.Duration
	// WindowBytes is the link's unacknowledged window. Zero takes
	// nodesocket.DefaultWindowBytes, which is sized under the socket buffers so
	// backpressure lands in the cancellable wait.
	WindowBytes int
	// AckInterval is the link's transport keepalive.
	AckInterval time.Duration
	// Journal records a node's arrival, its departure and every change to what
	// it claims. nil discards them.
	//
	// #170 says disconnects are journalled, NOT announced, and the reason is
	// worth keeping next to the field: a laptop that sleeps at six o'clock
	// disconnects every evening, and a nightly DM about it trains the one admin
	// who would act on the message that matters to ignore it.
	Journal journal.Recorder
	// Pins remembers which node a conversation was delegated to, so turn two
	// lands where turn one did and the choice survives a restart or a failover.
	//
	// nil disables pinning, and the degradation is honest rather than silent: a
	// conversation is re-elected on every cold session, which shows up as the
	// round robin moving a conversation between nodes. Delegation itself still
	// works, which is what makes a gateway without a pin store worth running at
	// all — and every gateway that opens the node endpoint opens this too.
	Pins config.ConversationPinStore
	// Background receives events belonging to a session rather than a turn.
	Background func(sessionID string, ev agent.Event)
	// Approve answers a native agent's inline tool approval. It is set by the
	// runtime builder, which is the only thing that knows the gateway's
	// per-agent approval gates, and may be replaced by a configuration reload.
	Approve func(ctx context.Context, toolName, summary string) (bool, string)
	// Advertise is the address, or the space- or comma-separated addresses,
	// operators tell nodes to use, replacing what this machine can work out
	// about itself. Several because #170 wants a name AND an address offered,
	// and behind a TLS terminator neither of them is discoverable from here.
	// Empty means "work it out" — see Host.Address, which explains why the
	// worked-out answer is ws:// and when that is not good enough.
	Advertise string
	// References is every agent profile NAME the gateway's own configuration
	// mentions. It is a function rather than a value because a configuration
	// reload rebuilds the gateway while the connections survive.
	//
	// It is what #198 moved to connect time: a gateway holds no profile bodies,
	// so it cannot resolve one of these names until a node says what it serves.
	// nil skips the check. See onboard.go.
	References func() []config.AgentReference
	// OnUnconfiguredNode is called when a node attaches claiming nothing.
	//
	// A node with no profiles has never been configured, which #170 Change I
	// makes a trigger for the EXISTING Slack onboarding rather than an error —
	// run against the node's OWNER, who is on the credential. The node has no
	// Slack of its own, which is why this is driven from the gateway. nil means
	// the arrival is journalled and nothing else.
	OnUnconfiguredNode func(ctx context.Context, node Node)
	// OnNodeSettled is called when a node stops being one with nothing
	// configured: it advertised something, or it disconnected.
	//
	// It is the other end of OnUnconfiguredNode and exists because the offer it
	// makes is an ENTITLEMENT held on the Slack side, not a message. An offer
	// that is never withdrawn outlives its node for the life of the process, and
	// the gateway routes its owner's next click at a node id that is configured
	// or gone. nil means an offer is only ever ended by being used.
	OnNodeSettled func(node Node)
}

// Host owns the accept endpoint and the registry of connected nodes.
type Host struct {
	tokens config.NodeTokenStore
	log    *slog.Logger
	now    func() time.Time
	opts   Options
	rec    journal.Recorder

	// conns numbers connections. It names a socket and never crosses the wire.
	conns atomic.Int64
	// cursor is the delegation round robin. It lives here, not in the runtime
	// builder's closure, because a configuration reload re-runs that builder
	// while the connections survive — see roundRobin.
	cursor atomic.Uint64
	// pins is where an election is written down. Fixed for the Host's life: it
	// is a database handle, not a live gateway reference.
	pins config.ConversationPinStore

	mu sync.Mutex
	// leadership is the election this Host defers to before accepting anything.
	// nil until FollowLeader is called, and nil accepts nothing — see leader.go.
	leadership Leadership
	// listen is the address the listener actually bound, which is what gets
	// published for standbys to redirect to. Empty until serving starts.
	listen string
	// nodes is the registry, keyed per CONNECTION. See registry.go for why that
	// is not per node, and why enumeration collapses the other way.
	nodes map[string]*attached
	// sessions binds an agent session id to the connection that minted it.
	//
	// It is what makes a warm turn stay on its node without re-reading the pin:
	// the session manager caches conversation → session id and calls Prompt
	// with only the id, so this is the only place the id can be resolved back
	// to a machine. An id whose connection has gone is ErrSessionGone, which
	// the manager answers by opening a fresh session — and THAT is what runs
	// the re-election.
	sessions map[string]*attached
	// takeovers marks the sessions whose first prompt must tell the model the
	// conversation moved. Keyed by session id, valued by the node it came from,
	// consumed on use. See takeover.go.
	takeovers map[string]string
	// access carries the node grants. Live rather than captured, like the
	// approver: a gateway admin adding a grant must take effect on the next
	// election and not on the next restart.
	access     config.AccessConfig
	approve    func(ctx context.Context, toolName, summary string) (bool, string)
	background func(sessionID string, ev agent.Event)
	// tools is the gateway's registry, from which a node is served the
	// node-reachable slice. Live rather than captured, for the reason the
	// approver is: a configuration reload rebuilds it under a surviving node
	// connection.
	tools *tools.Registry
}

// New builds a Host. It listens for nothing until Listen or Handler is used.
func New(opts Options) (*Host, error) {
	if opts.Tokens == nil {
		return nil, errors.New("nodehost: no node credential store")
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	var rec journal.Recorder = journal.NopRecorder{}
	if opts.Journal != nil {
		rec = opts.Journal
	}
	return &Host{
		tokens:     opts.Tokens,
		log:        log,
		now:        now,
		opts:       opts,
		rec:        rec,
		pins:       opts.Pins,
		nodes:      make(map[string]*attached),
		sessions:   make(map[string]*attached),
		approve:    opts.Approve,
		background: opts.Background,
	}, nil
}

// Handler answers the node endpoint and nothing else.
//
// It is a ServeMux rather than a bare handler so any other path on this
// listener is a 404 instead of an upgrade attempt: the port exists for one
// purpose and should say so.
func (h *Host) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(nodesocket.Path, h.serveLink)
	return mux
}

// Listen serves the node endpoint until ctx ends.
//
// Plain HTTP, and settled rather than deferred. #170 makes wss mandatory and
// this process does not provide it: terminating TLS here would mean owning
// certificate loading, renewal and pinning, and the deployments that need it
// already have something that does all three. So the two supported shapes are
// loopback — the `--role both` node the split is exercised with — and a reverse
// proxy holding the certificate, whose address is what -node-advertise names.
//
// The dialler enforces the other half: it refuses plain ws:// to anything but a
// loopback host, for a LEARNED address exactly as for a configured one. That is
// what keeps the redirect from becoming a way to talk a node into putting its
// credential on the wire in cleartext.
func (h *Host) Listen(ctx context.Context, addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("nodehost: listen on %s: %w", addr, err)
	}
	return h.Serve(ctx, listener)
}

// Serve is Listen over a listener the caller already holds — which is what a
// test needs to be handed the port the kernel chose.
func (h *Host) Serve(ctx context.Context, listener net.Listener) error {
	server := &http.Server{
		Handler: h.Handler(),
		// A node holds its connection open for days, so no read or write
		// timeout can be set here without killing healthy links. The upgrade
		// itself is bounded instead.
		ReadHeaderTimeout: 10 * time.Second,
	}
	h.setListenAddr(listener.Addr().String())
	// Forgotten again however serving ends. What this publishes is where nodes
	// can be accepted, and an address that outlived its listener is one every
	// standby in the fleet keeps redirecting nodes to.
	defer h.setListenAddr("")
	h.log.Info("runtime node endpoint listening", "addr", listener.Addr().String(), "path", nodesocket.Path)

	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()

	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		h.DetachAll("the node endpoint is shutting down")
		return nil
	}
}

// serveLink authenticates one node and serves its connection to completion.
func (h *Host) serveLink(w http.ResponseWriter, r *http.Request) {
	token, ok := nodesocket.BearerToken(r)
	if !ok {
		http.Error(w, "a node token is required", http.StatusUnauthorized)
		return
	}
	record, err := nodetoken.Verify(r.Context(), h.tokens, token, h.now())
	switch {
	case errors.Is(err, nodetoken.ErrNotAuthorized):
		// The reason is logged, never returned: the peer already knows what it
		// presented, and telling it which of "unknown", "revoked" and "wrong
		// secret" applies is free reconnaissance.
		h.log.Warn("runtime node rejected", "error", err, "remote", r.RemoteAddr)
		http.Error(w, "not authorized", http.StatusUnauthorized)
		return
	case err != nil:
		// A store that is down has not said the credential is bad. Answering
		// 401 would make a database outage look like a fleet-wide revocation.
		h.log.Error("could not check a node credential", "error", err)
		http.Error(w, "the credential store is unavailable", http.StatusServiceUnavailable)
		return
	}

	// Leadership is checked AFTER the credential and BEFORE the upgrade. After,
	// so the redirect names the leader only to a node entitled to know; before,
	// because a refusal that arrives as a close frame arrives as bare EOF — the
	// transport collapses every ordinary close code and drops the reason text,
	// and the node would redial the same address forever. See leader.go.
	if leadership, ok := h.leading(r.Context()); !ok {
		h.refuseNotLeading(w, r, leadership, record.NodeID)
		return
	}

	conn, err := nodesocket.Upgrade(w, r, h.opts.WriteTimeout)
	if err != nil {
		h.log.Warn("runtime node upgrade failed", "error", err, "node_id", record.NodeID)
		return
	}

	window := h.opts.WindowBytes
	if window <= 0 {
		window = nodesocket.DefaultWindowBytes
	}
	log := h.log.With("node_id", record.NodeID)

	// The registry entry is built BEFORE the client, not after, and the order
	// is load-bearing. Two things the node does during the handshake have to
	// reach a specific connection rather than "the node": its opening claim,
	// which arrives from inside Initialize, and its tool calls, whose turn is
	// looked up on the client that made them. Building the entry afterwards
	// would leave both with nothing to name.
	node := &attached{
		connID:     strconv.FormatInt(h.conns.Add(1), 10),
		selector:   record.Selector,
		nodeID:     record.NodeID,
		userID:     record.UserID,
		attachedAt: h.now(),
		closed:     make(chan struct{}),
	}
	client := remote.New(conn, remote.Options{
		Logger:     log,
		Background: h.deliverBackground,
		Approve:    h.askApproval,
		// Murtaugh's own tools, served back down the connection the node
		// dialled — never a second one. The gateway still never dials a node.
		Tools: &toolServer{host: h, node: node, log: log},
		// What this node says it can serve. Bound per connection because that
		// is what the claim belongs to: a node holding two live credentials
		// through a rotation advertises on each of them.
		Advertise:   remote.AdvertiserFunc(func(ad agentwire.Advertisement) { h.advertise(node, ad) }),
		WindowBytes: window,
		AckInterval: h.opts.AckInterval,
	})
	node.client = client

	// Initialized here rather than lazily on the first turn, because the
	// gateway's session manager latches "initialized" and would never retry it
	// after a reconnect. A node that cannot bring its agent up is not published.
	initCtx, cancel := context.WithTimeout(r.Context(), initializeTimeout)
	err = client.Initialize(initCtx)
	cancel()
	if err != nil {
		log.Error("runtime node could not initialize its agent", "error", err)
		_ = client.Close()
		return
	}

	h.attach(node)

	// Holding the HTTP handler goroutine for the connection's life is
	// deliberate: it keeps the server's own connection accounting honest, and
	// it means there is exactly one goroutine per node to reason about.
	select {
	case <-client.Done():
	case <-node.closed:
	}
	h.detach(node)
}

// attach publishes a connection to the registry.
//
// The opening claim is already on the entry: it arrived from inside Initialize,
// so a node is never published in a state where the gateway knows it is there
// and not what it serves.
func (h *Host) attach(node *attached) {
	displaced := h.insert(node)
	if displaced != nil {
		// Same credential, so this is the same node dialling back in — very
		// often over a socket the gateway is still holding and has not noticed
		// is dead. Replacing rather than refusing is what lets it recover
		// without waiting for a TCP timeout. A DIFFERENT credential is a
		// rotation and both connections stay; see Host.insert.
		h.log.Info("a runtime node redialled on the same credential; replacing the previous connection",
			"node_id", displaced.nodeID, "selector", displaced.selector)
		displaced.close()
	}
	h.mu.Lock()
	ad := node.ad.Clone()
	count := len(h.nodes)
	h.mu.Unlock()
	h.log.Info("runtime node attached", "node_id", node.nodeID, "user_id", node.userID, "selector", node.selector,
		"profiles", len(ad.Profiles), "claims", len(ad.Claims), "connected", count)
	h.record(journal.LevelInfo, "attached", "A runtime node attached", node, ad)
	// After the entry is published, never before: the fleet-scoped check below
	// reads the registry, and a node reviewing its own arrival before it is in
	// there would be told its own profiles are not served.
	h.reviewClaim(node, ad)
}

func (h *Host) detach(node *attached) {
	h.remove(node)
	node.close()
	h.log.Info("runtime node detached", "node_id", node.nodeID, "selector", node.selector)
	// Journalled, never announced. See Options.Journal.
	h.record(journal.LevelInfo, "detached", "A runtime node disconnected", node, agentwire.Advertisement{})
	// A node that is gone cannot be configured by a form, so its owner's
	// invitation ends with the connection. It comes back on the next attach if
	// the node is still unconfigured, which is seconds away.
	h.settle(node)
}

// advertise records a node's claim, at the handshake and on every later change.
//
// Nothing here decides anything: delegation (#196) reads the registry, and the
// staleness this design accepts is one configuration edit wide — its worst
// outcome is one conversation delegated to the wrong node. The alternative #170
// rejects is asking every node at delegation time, which puts an N-way fan-out
// on the first message of every conversation, where one wedged node adds a
// timeout to every delegation in the workspace.
func (h *Host) advertise(node *attached, ad agentwire.Advertisement) {
	if !h.setAdvertisement(node, ad) {
		// The opening claim, arriving from inside the handshake. attach records
		// it as part of the arrival; a second journal line saying the same
		// thing is the noise that makes a journal unreadable.
		return
	}
	h.log.Info("a runtime node changed what it claims", "node_id", node.nodeID,
		"profiles", len(ad.Profiles), "claims", len(ad.Claims))
	h.record(journal.LevelInfo, "advertised", "A runtime node changed what it claims", node, ad)
	// A claim change can settle either of the two questions differently: a node
	// that finished onboarding stops being unconfigured, and one whose owner
	// renamed a profile can strand a gateway reference that resolved a minute
	// ago. Both are worth knowing at the moment they become true.
	h.reviewClaim(node, ad)
}

// record puts one node lifecycle event on the gateway stream.
//
// The stream is the gateway's because that is whose fleet this is, and the kind
// is `node` so a disconnect at 18:00 every evening is one query away rather than
// one DM away.
func (h *Host) record(level journal.Level, state, summary string, node *attached, ad agentwire.Advertisement) {
	payload := map[string]any{
		"state":    state,
		"node_id":  node.nodeID,
		"selector": node.selector,
	}
	if !ad.Empty() {
		payload["profiles"] = ad.Profiles
		claims := make([]string, 0, len(ad.Claims))
		for _, claim := range ad.Claims {
			claims = append(claims, claim.Match)
		}
		payload["claims"] = claims
	}
	h.rec.Record(context.Background(), journal.Event{
		Stream:  journal.StreamGateway,
		Kind:    "node",
		Level:   level,
		Summary: summary,
		Keys:    journal.Keys{UserID: node.userID},
		Payload: payload,
	})
}

// close ends one connection. It is safe to call concurrently and repeatedly,
// which is a requirement rather than tidiness: see attached.closeOnce for the
// four callers and what a check-then-close costs when two of them race.
func (n *attached) close() {
	n.closeOnce.Do(func() { close(n.closed) })
	// Outside the Once because remote.Client.Close already latches and a second
	// caller is a no-op; what has to happen exactly once is the channel close.
	_ = n.client.Close()
}

// anyClient answers the questions that are about the FLEET rather than about a
// conversation: is there anything attached at all, and does it interrupt.
//
// It is not delegation and must never be used as it. A conversation's node is
// chosen by delegate and then addressed through its session binding; the two
// callers here — the session manager's initialize probe and its interruptible
// probe — ask a question no particular node owns, and answering with the most
// recently attached one is as good as answering with any.
func (h *Host) anyClient() (*remote.Client, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	node := h.newest()
	if node == nil {
		return nil, ErrNoNode
	}
	return node.client, nil
}

// bindSession records which connection minted a session id.
func (h *Host) bindSession(sessionID string, node *attached) {
	if sessionID == "" {
		return
	}
	h.mu.Lock()
	h.sessions[sessionID] = node
	h.mu.Unlock()
}

// sessionNode resolves a session id back to the connection that minted it.
//
// An id this gateway never minted, or one whose connection has since gone, is
// agent.ErrSessionGone rather than ErrNoNode, and the distinction is the whole
// recovery path: ErrNoNode says the turn cannot run, while ErrSessionGone says
// this SESSION cannot run and a new one can. The session manager answers the
// second by discarding the binding and opening a fresh session, which is what
// re-runs the election and overwrites the stale pin.
//
// The entry is checked against the live registry rather than trusted, because a
// disconnect races a turn: the map is pruned on detach, but a lookup that
// happened to read the pointer first would otherwise hand a turn to a dead
// link and wait for its write timeout.
func (h *Host) sessionNode(sessionID string) (*attached, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	node, ok := h.sessions[sessionID]
	if !ok {
		return nil, agent.ErrSessionGone
	}
	if h.nodes[node.connID] != node {
		delete(h.sessions, sessionID)
		return nil, agent.ErrSessionGone
	}
	return node, nil
}

// unbindSession forgets a session id the gateway has closed.
func (h *Host) unbindSession(sessionID string) {
	h.mu.Lock()
	delete(h.sessions, sessionID)
	h.mu.Unlock()
}

// pruneSessionsLocked drops every session id a departing connection minted.
// Callers hold the mutex.
//
// sessionNode would answer correctly without this — it re-checks the registry —
// but a gateway that never forgets is a gateway whose map grows for every
// session of every node that ever attached.
func (h *Host) pruneSessionsLocked(node *attached) {
	for id, held := range h.sessions {
		if held == node {
			delete(h.sessions, id)
			delete(h.takeovers, id)
		}
	}
}

// setAccess replaces the gateway access policy delegation reads its grants
// from. Like the approver it is a live reference: a configuration reload
// rebuilds the gateway while the node connections survive, and a grant added by
// the gateway admin must reach the next election.
func (h *Host) setAccess(access config.AccessConfig) {
	h.mu.Lock()
	h.access = access
	h.mu.Unlock()
}

// accessConfig reads the current access policy.
func (h *Host) accessConfig() config.AccessConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.access
}

// SetApprover replaces the tool-approval gate the attached node's native agent
// reaches. A configuration reload rebuilds the gateway and its gates while the
// node connection survives, so the gate is a live reference rather than
// something captured when the connection was made.
func (h *Host) SetApprover(approve func(ctx context.Context, toolName, summary string) (bool, string)) {
	h.mu.Lock()
	h.approve = approve
	h.mu.Unlock()
}

// setBackground replaces the sink a node's background events are rendered
// through. Like the approver it is a live reference: a configuration reload
// rebuilds the gateway's background router while the node connection survives.
func (h *Host) setBackground(sink func(sessionID string, ev agent.Event)) {
	h.mu.Lock()
	h.background = sink
	h.mu.Unlock()
}

// deliverBackground hands an event to the current sink.
//
// It runs on the link's read loop, so the sink must not render into Slack
// synchronously — the gateway's background router queues per session for
// exactly that reason. One stretch's Slack write would otherwise stall frame
// delivery for every conversation on the link, not just its own.
func (h *Host) deliverBackground(sessionID string, ev agent.Event) {
	h.mu.Lock()
	sink := h.background
	h.mu.Unlock()
	if sink == nil {
		h.log.Warn("a node sent a background event with nothing bound to render it", "session_id", sessionID)
		return
	}
	sink(sessionID, ev)
}

func (h *Host) askApproval(ctx context.Context, toolName, summary string) (bool, string) {
	h.mu.Lock()
	approve := h.approve
	h.mu.Unlock()
	if approve == nil {
		return false, "Skipped: this gateway has no approval gate configured, so nobody could be asked. The action was not run."
	}
	return approve(ctx, toolName, summary)
}

// CloseCredential closes every connection authenticated with one credential.
//
// This is nodetoken.ConnectionCloser: it is what turns revocation from "the
// next handshake fails" into "the live connection ends", which is what #170
// says revocation means.
//
// It closes every connection the registry holds on that selector. There can
// only be one — see Host.takeCredential for why, and for the revocation hole
// that is genuinely open.
func (h *Host) CloseCredential(_ context.Context, selector string) error {
	for _, node := range h.takeCredential(selector) {
		h.log.Info("closing a runtime node whose credential was revoked", "node_id", node.nodeID, "selector", selector)
		h.record(journal.LevelWarn, "revoked", "A runtime node's credential was revoked and its connection closed", node, agentwire.Advertisement{})
		node.close()
	}
	return nil
}

var _ nodetoken.ConnectionCloser = (*Host)(nil)
