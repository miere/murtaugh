package nodehost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agent/remote"
	"github.com/miere/murtaugh/internal/config"
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
	// Background receives events belonging to a session rather than a turn.
	Background func(sessionID string, ev agent.Event)
	// Approve answers a native agent's inline tool approval. It is set by the
	// runtime builder, which is the only thing that knows the gateway's
	// per-agent approval gates, and may be replaced by a configuration reload.
	Approve func(ctx context.Context, toolName, summary string) (bool, string)
}

// Host owns the accept endpoint and the attached node.
type Host struct {
	tokens config.NodeTokenStore
	log    *slog.Logger
	now    func() time.Time
	opts   Options

	mu         sync.Mutex
	current    *attached
	approve    func(ctx context.Context, toolName, summary string) (bool, string)
	background func(sessionID string, ev agent.Event)
	// tools is the gateway's registry, from which a node is served the
	// node-reachable slice. Live rather than captured, for the reason the
	// approver is: a configuration reload rebuilds it under a surviving node
	// connection.
	tools *tools.Registry
}

// attached is one live node connection.
type attached struct {
	client   *remote.Client
	selector string
	nodeID   string
	closed   chan struct{}
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
	return &Host{
		tokens:     opts.Tokens,
		log:        log,
		now:        now,
		opts:       opts,
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
// Plain HTTP. #170 makes wss mandatory and this does not provide it: terminating
// TLS here would mean owning certificate loading, renewal and pinning, which is
// item 11's work and is the part the spec says to try on a real machine. Until
// then the honest deployment is loopback — the `--role both` node the split is
// exercised with — or a reverse proxy that already holds a certificate. The
// dialler enforces the other half of this: it refuses plain ws:// to anything
// but a loopback host.
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
		h.detachAll()
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

	conn, err := nodesocket.Upgrade(w, r, h.opts.WriteTimeout)
	if err != nil {
		h.log.Warn("runtime node upgrade failed", "error", err, "node_id", record.NodeID)
		return
	}

	window := h.opts.WindowBytes
	if window <= 0 {
		window = nodesocket.DefaultWindowBytes
	}
	client := remote.New(conn, remote.Options{
		Logger:     h.log.With("node_id", record.NodeID),
		Background: h.deliverBackground,
		Approve:    h.askApproval,
		// Murtaugh's own tools, served back down the connection the node
		// dialled — never a second one. The gateway still never dials a node.
		Tools:       &toolServer{host: h, log: h.log.With("node_id", record.NodeID)},
		WindowBytes: window,
		AckInterval: h.opts.AckInterval,
	})

	// Initialized here rather than lazily on the first turn, because the
	// gateway's session manager latches "initialized" and would never retry it
	// after a reconnect. A node that cannot bring its agent up is not published.
	initCtx, cancel := context.WithTimeout(r.Context(), initializeTimeout)
	err = client.Initialize(initCtx)
	cancel()
	if err != nil {
		h.log.Error("runtime node could not initialize its agent", "error", err, "node_id", record.NodeID)
		_ = client.Close()
		return
	}

	node := h.attach(client, record)
	h.log.Info("runtime node attached", "node_id", record.NodeID, "user_id", record.UserID, "selector", record.Selector)

	// Holding the HTTP handler goroutine for the connection's life is
	// deliberate: it keeps the server's own connection accounting honest, and
	// it means there is exactly one goroutine per node to reason about.
	select {
	case <-client.Done():
	case <-node.closed:
	}
	h.detach(node)
	h.log.Info("runtime node detached", "node_id", record.NodeID)
}

func (h *Host) attach(client *remote.Client, record config.NodeToken) *attached {
	node := &attached{client: client, selector: record.Selector, nodeID: record.NodeID, closed: make(chan struct{})}
	h.mu.Lock()
	previous := h.current
	h.current = node
	h.mu.Unlock()
	if previous != nil {
		// One slot, so the newcomer wins. When there is a registry this becomes
		// an insert; until then, replacing rather than refusing is what lets a
		// node that reconnected after a network drop take over from the
		// half-dead connection the gateway has not noticed yet.
		h.log.Warn("a second runtime node connected; replacing the first", "replaced", previous.nodeID, "with", record.NodeID)
		previous.close()
	}
	return node
}

func (h *Host) detach(node *attached) {
	h.mu.Lock()
	if h.current == node {
		h.current = nil
	}
	h.mu.Unlock()
	node.close()
}

func (h *Host) detachAll() {
	h.mu.Lock()
	node := h.current
	h.current = nil
	h.mu.Unlock()
	if node != nil {
		node.close()
	}
}

func (n *attached) close() {
	select {
	case <-n.closed:
	default:
		close(n.closed)
	}
	_ = n.client.Close()
}

// Attached reports the node currently serving, if any.
func (h *Host) Attached() (nodeID string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.current == nil {
		return "", false
	}
	return h.current.nodeID, true
}

// client returns the attached node's client, or ErrNoNode.
func (h *Host) client() (*remote.Client, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.current == nil {
		return nil, ErrNoNode
	}
	return h.current.client, nil
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
func (h *Host) CloseCredential(_ context.Context, selector string) error {
	h.mu.Lock()
	node := h.current
	if node != nil && node.selector == selector {
		h.current = nil
	} else {
		node = nil
	}
	h.mu.Unlock()
	if node == nil {
		return nil
	}
	h.log.Info("closing a runtime node whose credential was revoked", "node_id", node.nodeID, "selector", selector)
	node.close()
	return nil
}

var _ nodetoken.ConnectionCloser = (*Host)(nil)
