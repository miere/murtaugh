package nodehost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agent/remote"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/journal"
	"github.com/miere/murtaugh/internal/nodesocket"
	"github.com/miere/murtaugh/internal/nodetoken"
)

var ErrNoNode = errors.New("no runtime node is connected")

const (
	initializeTimeout = 30 * time.Second
	shutdownGrace     = 5 * time.Second
)

type Options struct {
	Tokens       config.NodeTokenStore
	Logger       *slog.Logger
	Now          func() time.Time
	WriteTimeout time.Duration
	WindowBytes  int
	AckInterval  time.Duration
	// Journalled, not announced: a laptop sleeping every evening would send a
	// nightly DM that trains the admin to ignore the one that matters.
	Journal    journal.Recorder
	Pins       config.ConversationPinStore
	Background func(sessionID string, ev agent.Event)
	Approve    func(ctx context.Context, toolName, summary string) (bool, string)
	// Behind a TLS terminator the gateway cannot discover its own name or address,
	// so operators set them here.
	Advertise string
	// A function, not a value, because a configuration reload rebuilds the gateway
	// while the node connections survive.
	References func() []config.AgentReference
	// Driven from the gateway because the node has no Slack connection of its own.
	OnUnconfiguredNode func(ctx context.Context, node Node)
	// The onboarding offer is an entitlement, not a message: never withdrawn, it
	// routes the owner's next click at a node that is configured or gone.
	OnNodeSettled func(node Node)

	RecheckInterval time.Duration
}

type Host struct {
	tokens config.NodeTokenStore
	log    *slog.Logger
	now    func() time.Time
	opts   Options
	rec    journal.Recorder

	conns  atomic.Int64
	cursor atomic.Uint64
	pins   config.ConversationPinStore

	mu         sync.Mutex
	leadership Leadership
	listen     string
	nodes      map[string]*attached
	sessions   map[string]*attached
	takeovers  map[string]string
	access     config.AccessConfig
	approve    func(ctx context.Context, toolName, summary string) (bool, string)
	background func(sessionID string, ev agent.Event)
	signIns    func(ctx context.Context, prompt *agent.SignInPrompt, settled <-chan agent.SignInSettled, shown func(error))
	health     func(agentruntime.CredentialHealth)
}

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

// A ServeMux, so any other path on this port is a 404 instead of an upgrade
// attempt.
func (h *Host) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(nodesocket.Path, h.serveLink)
	return mux
}

// Plain HTTP on purpose: TLS is left to a reverse proxy, and the dialler
// refuses ws:// to anything but loopback.
func (h *Host) Listen(ctx context.Context, addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("nodehost: listen on %s: %w", addr, err)
	}
	return h.Serve(ctx, listener)
}

func (h *Host) Serve(ctx context.Context, listener net.Listener) error {
	server := &http.Server{
		Handler:           h.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	h.setListenAddr(listener.Addr().String())
	defer h.setListenAddr("")
	h.log.Info("runtime node endpoint listening", "addr", listener.Addr().String(), "path", nodesocket.Path)

	watchCtx, stopWatching := context.WithCancel(ctx)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		h.watchCredentials(watchCtx)
	}()
	defer func() {
		stopWatching()
		<-watched
	}()

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

func (h *Host) serveLink(w http.ResponseWriter, r *http.Request) {
	token, ok := nodesocket.BearerToken(r)
	if !ok {
		http.Error(w, "a node token is required", http.StatusUnauthorized)
		return
	}
	record, err := nodetoken.Verify(r.Context(), h.tokens, token, h.now())
	switch {
	case errors.Is(err, nodetoken.ErrNotAuthorized):
		h.log.Warn("runtime node rejected", "error", err, "remote", r.RemoteAddr)
		http.Error(w, "not authorized", http.StatusUnauthorized)
		return
	case err != nil:
		h.log.Error("could not check a node credential", "error", err)
		http.Error(w, "the credential store is unavailable", http.StatusServiceUnavailable)
		return
	}

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

	node := &attached{
		connID:     strconv.FormatInt(h.conns.Add(1), 10),
		selector:   record.Selector,
		nodeID:     record.NodeID,
		userID:     record.UserID,
		attachedAt: h.now(),
		closed:     make(chan struct{}),
	}
	client := remote.New(conn, remote.Options{
		Logger:      log,
		Background:  func(sessionID string, ev agent.Event) { h.deliverBackground(node, sessionID, ev) },
		Approve:     h.askApproval,
		Advertise:   remote.AdvertiserFunc(func(ad agentwire.Advertisement) { h.advertise(node, ad) }),
		Owner:       record.UserID,
		SignIns:     h.drawSignIn,
		Credentials: func(report agentwire.CredentialHealth) { h.reportCredential(node, report) },
		WindowBytes: window,
		AckInterval: h.opts.AckInterval,
	})
	node.client = client

	initCtx, cancel := context.WithTimeout(r.Context(), initializeTimeout)
	err = client.Initialize(initCtx)
	cancel()
	if err != nil {
		log.Error("runtime node could not initialize its agent", "error", err)
		_ = client.Close()
		return
	}

	h.attach(node)

	select {
	case <-client.Done():
	case <-node.closed:
	}
	h.detach(node)
}

func (h *Host) attach(node *attached) {
	displaced := h.insert(node)
	if displaced != nil {
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
	if !config.IsSlackUserID(node.userID) {
		h.flagOwner(node)
	}
	h.reviewClaim(node, ad)
}

func (h *Host) detach(node *attached) {
	h.remove(node)
	node.close()
	h.log.Info("runtime node detached", "node_id", node.nodeID, "selector", node.selector)
	h.record(journal.LevelInfo, "detached", "A runtime node disconnected", node, agentwire.Advertisement{})
	h.settle(node)
}

func (h *Host) advertise(node *attached, ad agentwire.Advertisement) {
	if !h.setAdvertisement(node, ad) {
		return
	}
	h.log.Info("a runtime node changed what it claims", "node_id", node.nodeID,
		"profiles", len(ad.Profiles), "claims", len(ad.Claims))
	h.record(journal.LevelInfo, "advertised", "A runtime node changed what it claims", node, ad)
	h.reviewClaim(node, ad)
}

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

func (h *Host) flagOwner(node *attached) {
	const fix = "re-mint this node's token with --user U… (the owner's Slack user ID)"
	h.log.Warn("a runtime node's token names an owner that is not a Slack user ID, so its sign-ins can reach nobody; "+fix,
		"node_id", node.nodeID, "user_id", node.userID, "selector", node.selector)
	h.rec.Record(context.Background(), journal.Event{
		Stream:  journal.StreamGateway,
		Kind:    "node",
		Level:   journal.LevelWarn,
		Summary: "A runtime node's token names an owner that is not a Slack user ID",
		Keys:    journal.Keys{UserID: node.userID},
		Payload: map[string]any{"state": "owner_not_a_slack_user", "node_id": node.nodeID, "selector": node.selector, "fix": fix},
	})
}

func (n *attached) close() {
	n.closeOnce.Do(func() { close(n.closed) })
	_ = n.client.Close()
}

func (h *Host) anyClient() (*remote.Client, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	node := h.newest()
	if node == nil {
		return nil, ErrNoNode
	}
	return node.client, nil
}

func (h *Host) bindSession(sessionID string, node *attached) {
	if sessionID == "" {
		return
	}
	h.mu.Lock()
	h.sessions[sessionID] = node
	h.mu.Unlock()
}

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

func (h *Host) unbindSession(sessionID string) {
	h.mu.Lock()
	delete(h.sessions, sessionID)
	h.mu.Unlock()
}

func (h *Host) pruneSessionsLocked(node *attached) {
	for id, held := range h.sessions {
		if held == node {
			delete(h.sessions, id)
			delete(h.takeovers, id)
		}
	}
}

func (h *Host) setAccess(access config.AccessConfig) {
	h.mu.Lock()
	h.access = access
	h.mu.Unlock()
}

func (h *Host) accessConfig() config.AccessConfig {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.access
}

// A configuration reload rebuilds the gates while the node connection
// survives, so the gate must be a live reference.
func (h *Host) SetApprover(approve func(ctx context.Context, toolName, summary string) (bool, string)) {
	h.mu.Lock()
	h.approve = approve
	h.mu.Unlock()
}

func (h *Host) setBackground(sink func(sessionID string, ev agent.Event)) {
	h.mu.Lock()
	h.background = sink
	h.mu.Unlock()
}

func (h *Host) deliverBackground(from *attached, sessionID string, ev agent.Event) {
	if owner, err := h.sessionNode(sessionID); err != nil || owner.nodeID != from.nodeID {
		h.log.Warn("dropped a background event naming a session this node is not serving",
			"node_id", from.nodeID, "session_id", sessionID)
		return
	}
	h.mu.Lock()
	sink := h.background
	h.mu.Unlock()
	if sink == nil {
		h.log.Warn("a node sent a background event with nothing bound to render it", "session_id", sessionID)
		return
	}
	sink(sessionID, ev)
}

func (h *Host) setSignIns(draw func(ctx context.Context, prompt *agent.SignInPrompt, settled <-chan agent.SignInSettled, shown func(error))) {
	h.mu.Lock()
	h.signIns = draw
	h.mu.Unlock()
}

func (h *Host) drawSignIn(ctx context.Context, prompt *agent.SignInPrompt, settled <-chan agent.SignInSettled, shown func(error)) {
	h.mu.Lock()
	draw := h.signIns
	h.mu.Unlock()
	if draw == nil {
		shown(errors.New("this gateway cannot show a sign-in outside a conversation"))
		return
	}
	draw(ctx, prompt, settled, shown)
}

func (h *Host) setCredentialHealth(sink func(agentruntime.CredentialHealth)) {
	h.mu.Lock()
	h.health = sink
	h.mu.Unlock()
}

const maxCredentialsPerNode = 4

func (h *Host) reportCredential(node *attached, report agentwire.CredentialHealth) {
	health := agentruntime.CredentialHealth{
		NodeID:     node.nodeID,
		Owner:      node.userID,
		Credential: report.Credential,
		Degraded:   report.Degraded,
		Reason:     report.Reason,
		Since:      report.Since,
		ExpiresAt:  report.ExpiresAt,
		ReportedAt: h.now(),
	}
	h.mu.Lock()
	if node.credentials == nil {
		node.credentials = make(map[string]agentruntime.CredentialHealth)
	}
	_, known := node.credentials[report.Credential]
	full := !known && len(node.credentials) >= maxCredentialsPerNode
	if !full {
		node.credentials[report.Credential] = health
	}
	sink := h.health
	h.mu.Unlock()
	if full {
		h.log.Warn("ignored a report on one credential too many from a runtime node", "node_id", node.nodeID, "credential", report.Credential, "limit", maxCredentialsPerNode)
		return
	}
	if health.Degraded {
		h.log.Warn("a runtime node reports a failing credential", "node_id", node.nodeID, "credential", report.Credential, "reason", report.Reason)
	}
	if sink != nil {
		sink(health)
	}
}

func (h *Host) credentialReports() []agentruntime.CredentialHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []agentruntime.CredentialHealth
	for _, node := range h.collapsedLocked() {
		for _, report := range node.credentials {
			out = append(out, report)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeID != out[j].NodeID {
			return out[i].NodeID < out[j].NodeID
		}
		return out[i].Credential < out[j].Credential
	})
	return out
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

// Closes by selector, never by node: mid-rotation a node holds two live
// credentials and must keep the new one.
func (h *Host) CloseCredential(_ context.Context, selector string) error {
	h.closeCredential(selector, nodetoken.ErrRevoked)
	return nil
}

var _ nodetoken.ConnectionCloser = (*Host)(nil)
