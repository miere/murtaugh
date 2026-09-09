package nodehost_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	configstore "github.com/miere/murtaugh/internal/config/store"
	"github.com/miere/murtaugh/internal/journal"
	"github.com/miere/murtaugh/internal/nodehost"
	"github.com/miere/murtaugh/internal/nodeserve"
	"github.com/miere/murtaugh/internal/nodesocket"
	"github.com/miere/murtaugh/internal/nodetoken"
	"github.com/miere/murtaugh/internal/tools"
)

// This file is #193's verification, over a real WebSocket on loopback: a chat
// turn, a cancellation mid-turn, an approval round trip, and — in
// backpressure_test.go — the measurement.
//
// Everything below the test is production code. The gateway half is the real
// Host, the real token verification, the real remote client under a real
// agent.SessionManager built by the real runtime builder; the node half is the
// real nodeserve.Server and the real tool gate. Only two things are fakes: the
// agent (a scripted agent.Client, because a language model is not what is under
// test) and the credential store (in memory, because the SQL one has its own
// tests).

// loopback is one gateway and one node, attached over 127.0.0.1.
type loopback struct {
	host     *nodehost.Host
	store    *memTokens
	addr     string
	selector string
	// token is the credential this node dialled with, kept so a test can dial
	// the SAME node back in after a drop.
	token    string
	sessions map[string]*agent.SessionManager
	agent    *scriptedAgent
	gate     *nodeserve.ToolGate
	// background is the NODE's sink — what a backend on the node calls when it
	// emits outside a turn.
	background *nodeserve.BackgroundSink
	approved   chan approval
	// notices is what the GATEWAY's background hook received. It is the far end
	// of the same path.
	notices chan notice
	// proxy is the NODE's stand-in registry for Murtaugh's tools; nil unless the
	// rig was built with withTools. Its registry is what a node's agent would
	// have been built from.
	proxy *nodeserve.ToolProxy
	// claim is the NODE's advertiser — what it tells the gateway it serves. It
	// is always present, because a node that claims nothing is a real state and
	// not a disabled feature.
	claim *nodeserve.Advertiser
	// nodeStopped closes when the node's Serve returns, however it ended.
	nodeStopped chan struct{}
}

// approval records what the gateway's tool gate was asked, and answers it.
type approval struct {
	tool    string
	summary string
	answer  chan approvalAnswer
}

type approvalAnswer struct {
	allowed bool
	note    string
}

// notice is one event the gateway's background router was handed.
type notice struct {
	sessionID string
	event     agent.Event
}

// rigOption tunes the rig for the handful of tests that need something other
// than one healthy attached node.
type rigOption func(*rigConfig)

type rigConfig struct {
	waitForAttach bool
	// claim is what the node claims before it dials, so the handshake answer
	// carries it — which is the path a connect-time claim actually takes.
	claim agentwire.Advertisement
	// journal is the gateway's recorder. nil discards, which is what a Host
	// built without one does.
	journal journal.Recorder
	// registry is the GATEWAY's tool registry, and serving it is opt-in: a node
	// built with no ToolProxy never asks for a tool list, which is the state
	// every test above this one is in and the state a node was in before #194.
	registry *tools.Registry
	// pins is where delegation writes its choice. nil means the gateway does
	// not pin, which is a supported state: every conversation is re-elected on
	// its next cold session.
	pins config.ConversationPinStore
}

// withoutWaitingForAttach is for the tests whose point is that the node does
// NOT get published.
func withoutWaitingForAttach() rigOption {
	return func(c *rigConfig) { c.waitForAttach = false }
}

// withTools gives the gateway a registry and the node a proxy for it — the tool
// channel switched on.
func withTools(registry *tools.Registry) rigOption {
	return func(c *rigConfig) { c.registry = registry }
}

// claiming gives the node something to advertise before it dials.
func claiming(ad agentwire.Advertisement) rigOption {
	return func(c *rigConfig) { c.claim = ad }
}

// journalling gives the gateway a recorder, so a test can read what a node's
// arrival and departure wrote.
func journalling(rec journal.Recorder) rigOption {
	return func(c *rigConfig) { c.journal = rec }
}

// pinning gives the gateway somewhere to record which node it delegated a
// conversation to.
func pinning(pins config.ConversationPinStore) rigOption {
	return func(c *rigConfig) { c.pins = pins }
}

func dialLoopback(t *testing.T, script *scriptedAgent, options ...rigOption) *loopback {
	t.Helper()
	cfg := rigConfig{waitForAttach: true}
	for _, apply := range options {
		apply(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store := &memTokens{records: map[string]config.NodeToken{}}
	minted := mintInto(t, store, "node-1")

	approved := make(chan approval, 4)
	notices := make(chan notice, 8)
	host, err := nodehost.New(nodehost.Options{Tokens: store, Logger: testLogger(), Journal: cfg.journal, Pins: cfg.pins})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	// Every rig below is a gateway that LEADS. A Host accepts nothing until it
	// is told which election to defer to — see failover_test.go, where that
	// default is the thing under test.
	host.FollowLeader(elected{})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- host.Serve(ctx, listener) }()

	// The real builder, so the test exercises the wiring a gateway would use —
	// including the choice of which agent's approval gate a node reaches.
	agentCfg := config.Config{
		Agents: map[string]config.AgentProfile{"default": {}},
		Chat:   config.ChatConfig{Enabled: true, Defaults: config.ChatDefaults{Agent: "default"}},
	}
	runtime := nodehost.Runtime(host)(agentCfg, cfg.registry, testLogger())(agentruntime.Hooks{
		Chat: true,
		Approvers: map[string]agentruntime.Approver{
			"default": approverFunc(func(_ context.Context, tool, summary string) (bool, string) {
				ask := approval{tool: tool, summary: summary, answer: make(chan approvalAnswer, 1)}
				approved <- ask
				answer := <-ask.answer
				return answer.allowed, answer.note
			}),
		},
		// The gateway's background router in miniature: what a claude_code
		// stretch that finished after its turn would be rendered from.
		BackgroundEvents: func(sessionID string, ev agent.Event) {
			notices <- notice{sessionID: sessionID, event: ev}
		},
	})

	gate := nodeserve.NewToolGate(testLogger())
	background := nodeserve.NewBackgroundSink(testLogger())
	claim := nodeserve.NewAdvertiser(testLogger())
	// Set while unbound, exactly as cmd/murtaugh-runtime sets it before its
	// first dial: the value is held and the handshake answer carries it.
	claim.Publish(ctx, cfg.claim)
	var proxy *nodeserve.ToolProxy
	if cfg.registry != nil {
		proxy = nodeserve.NewToolProxy(testLogger())
		// What a node's runtime builder would be handed. The scripted agent
		// reaches for tools out of it exactly as a real backend's toolset does.
		script.tools = proxy.Registry()
	}
	script.gate = gate
	conn, err := nodesocket.Dial(ctx, "ws://"+listener.Addr().String(), nodesocket.DialOptions{Token: minted.Token})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// Closed rather than sent on, so a test can wait for the node's connection
	// to end and the cleanup can wait for the same thing.
	nodeStopped := make(chan struct{})
	go func() {
		defer close(nodeStopped)
		_ = nodeserve.Serve(ctx, conn, script, nodeserve.Options{
			Logger:      testLogger(),
			Gate:        gate,
			Background:  background,
			Tools:       proxy,
			Advertise:   claim,
			WindowBytes: nodesocket.DefaultWindowBytes,
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-nodeStopped
		<-served
	})

	if cfg.waitForAttach {
		waitFor(t, "the node to attach", func() bool {
			_, ok := host.Attached()
			return ok
		})
	}

	return &loopback{
		host:       host,
		store:      store,
		addr:       listener.Addr().String(),
		selector:   minted.Selector,
		token:      minted.Token,
		sessions:   runtime.Sessions,
		agent:      script,
		gate:       gate,
		background: background,
		approved:   approved,
		notices:    notices,
		proxy:      proxy,
		claim:      claim,

		nodeStopped: nodeStopped,
	}
}

// reload rebuilds the gateway's runtime over the SAME live node connection,
// which is what a configuration reload does: buildGateway runs again while the
// node's socket is untouched.
func (l *loopback) reload(t *testing.T, access config.AccessConfig) *agent.SessionManager {
	t.Helper()
	cfg := config.Config{
		Agents: map[string]config.AgentProfile{"default": {}},
		Chat:   config.ChatConfig{Enabled: true, Defaults: config.ChatDefaults{Agent: "default"}},
		Access: access,
	}
	rt := nodehost.Runtime(l.host)(cfg, nil, testLogger())(agentruntime.Hooks{Chat: true})
	manager := rt.Sessions["default"]
	if manager == nil {
		t.Fatal("the reloaded runtime built no session manager")
	}
	l.sessions = rt.Sessions
	return manager
}

// mintInto adds one usable credential to the store and returns it.
// nodeOwner is the Murtaugh user every credential in this file is minted for.
//
// It is named rather than spelled inline because delegation is keyed on it: a
// conversation is served by the initiating user's OWN nodes, so a turn whose
// metadata names somebody else is not a routing near-miss — it has no fleet at
// all and is refused. The tests below say so by using this constant on both
// sides.
const nodeOwner = "U-owner"

func mintInto(t *testing.T, store *memTokens, nodeID string) nodetoken.Minted {
	t.Helper()
	minted, err := nodetoken.Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := store.Put(context.Background(), config.NodeToken{
		Selector:   minted.Selector,
		SecretHash: string(minted.SecretHash),
		NodeID:     nodeID,
		UserID:     nodeOwner,
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("store credential: %v", err)
	}
	return minted
}

// 1. A chat turn, end to end against a connected runtime.
func TestAChatTurnIsServedByAConnectedNode(t *testing.T) {
	script := newScriptedAgent(func(turn *scriptedTurn) {
		turn.emit(agent.Event{Type: agent.EventStatus, Text: "thinking"})
		turn.emit(agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{
			ID: "t1", Title: "read a file", Status: agent.TaskStatusComplete,
		}})
		turn.emit(agent.Event{Type: agent.EventText, Text: "half a sentence "})
		turn.emit(agent.Event{Type: agent.EventText, Text: "and the rest.\n"})
		turn.emit(agent.Event{Type: agent.EventComplete, StopReason: "end_turn"})
	})
	rig := dialLoopback(t, script)

	manager := rig.sessions["default"]
	if manager == nil {
		t.Fatal("the runtime built no session manager for the default agent")
	}
	key := agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"}
	events, err := manager.Prompt(context.Background(), key,
		agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4", UserID: nodeOwner},
		agent.PromptRequest{Text: "hello", Channel: "C1", Thread: "123.4", User: "U9"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}

	var kinds []agent.EventType
	var prose string
	for ev := range events {
		kinds = append(kinds, ev.Type)
		if ev.Type == agent.EventText {
			prose += ev.Text
		}
		if ev.Type == agent.EventError {
			t.Fatalf("the turn failed: %v", ev.Error)
		}
		if ev.Type == agent.EventTask && (ev.Task == nil || ev.Task.Title != "read a file") {
			t.Fatalf("the task lost its identity crossing the wire: %+v", ev.Task)
		}
	}
	if prose != "half a sentence and the rest.\n" {
		t.Fatalf("the reply came back as %q", prose)
	}
	if len(kinds) != 5 {
		t.Fatalf("the turn produced %v, want five events in order", kinds)
	}
	if got := script.lastPrompt(); got.Text != "hello" || got.Thread != "123.4" {
		t.Fatalf("the node was prompted with %+v", got)
	}
}

// 2. A cancellation mid-turn. The gateway's idle path and its /stop command
// both do `cancel(); for range events {}` and block until the channel closes;
// across a link the context reaches nothing, so the cancel has to travel.
func TestCancellationMidTurnReachesTheNodeAndClosesTheStream(t *testing.T) {
	started := make(chan struct{})
	script := newScriptedAgent(func(turn *scriptedTurn) {
		turn.emit(agent.Event{Type: agent.EventText, Text: "working"})
		close(started)
		// Ends only when the node's backend is interrupted.
		<-turn.cancelled
		turn.emit(agent.Event{Type: agent.EventError, Error: context.Canceled})
	})
	rig := dialLoopback(t, script)

	manager := rig.sessions["default"]
	key := agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"}
	events, err := manager.Prompt(context.Background(), key,
		agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4", UserID: nodeOwner},
		agent.PromptRequest{Text: "long job", Channel: "C1", Thread: "123.4"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if first := receiveEvent(t, events); first.Text != "working" {
		t.Fatalf("first event was %+v", first)
	}
	<-started

	sessionID, ok := manager.Lookup(key)
	if !ok {
		t.Fatal("the manager did not record the node's session")
	}
	cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.Cancel(cancelCtx, sessionID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// The interrupt has to reach the node's backend, and the resulting error
	// must still satisfy the identity check the renderer branches on — a
	// cancellation that arrives as a generic failure becomes a failure card
	// instead of an "interrupted" seal.
	sawCancelled := false
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for ev := range events {
			if ev.Type == agent.EventError && errors.Is(ev.Error, context.Canceled) {
				sawCancelled = true
			}
		}
	}()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn's channel never closed after a cancel — the gateway would hang for its full idle timeout")
	}
	if !sawCancelled {
		t.Fatal("the cancellation lost its identity crossing the wire")
	}
	if n := script.cancels(); n != 1 {
		t.Fatalf("the node's backend was cancelled %d times, want 1", n)
	}
}

// 3. An approval round trip: the node's native tool gate, the gateway's human,
// and the note that comes back — which for a native tool call is not
// diagnostics but the result string the model is handed.
func TestApprovalRoundTripCarriesTheDecisionAndTheNote(t *testing.T) {
	outcome := make(chan approvalAnswer, 1)
	script := newScriptedAgent(func(turn *scriptedTurn) {
		allowed, note := turn.gate.Approve(turn.ctx, "terminal", "rm -rf /tmp/x")
		outcome <- approvalAnswer{allowed: allowed, note: note}
		turn.emit(agent.Event{Type: agent.EventText, Text: "not run"})
	})
	rig := dialLoopback(t, script)

	manager := rig.sessions["default"]
	events, err := manager.Prompt(context.Background(),
		agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"},
		agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4", UserID: nodeOwner},
		agent.PromptRequest{Text: "delete it", Channel: "C1", Thread: "123.4", User: "U9"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}

	var ask approval
	select {
	case ask = <-rig.approved:
	case <-time.After(10 * time.Second):
		t.Fatal("the gateway's approval gate was never asked")
	}
	if ask.tool != "terminal" || ask.summary != "rm -rf /tmp/x" {
		t.Fatalf("the gate was asked about %+v", ask)
	}
	ask.answer <- approvalAnswer{allowed: false, note: "Denied by the user. The action was not run; do not retry it without their go-ahead."}

	select {
	case got := <-outcome:
		if got.allowed {
			t.Fatal("a denied tool call came back as allowed")
		}
		if got.note != "Denied by the user. The action was not run; do not retry it without their go-ahead." {
			t.Fatalf("the note did not survive the round trip: %q", got.note)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the node's tool gate never got its answer")
	}

	// The approval is a request, not an event: rendering it would give a remote
	// native agent a card the local one does not have.
	for ev := range events {
		if ev.Type == agent.EventPermission {
			t.Fatal("a native tool approval reached the renderer as an event")
		}
	}
}

// A turn torn down with an approval outstanding must answer it. claude_code's
// control request waits on its process exiting rather than on the turn's
// context, so an unanswered approval parks a backend goroutine until the agent
// is killed.
func TestATornDownTurnAnswersItsOutstandingApproval(t *testing.T) {
	outcome := make(chan approvalAnswer, 1)
	asked := make(chan struct{})
	script := newScriptedAgent(func(turn *scriptedTurn) {
		go func() {
			allowed, note := turn.gate.Approve(turn.ctx, "terminal", "rm -rf /tmp/x")
			outcome <- approvalAnswer{allowed: allowed, note: note}
		}()
		<-asked
		<-turn.cancelled
	})
	rig := dialLoopback(t, script)

	manager := rig.sessions["default"]
	key := agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"}
	events, err := manager.Prompt(context.Background(), key,
		agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4", UserID: nodeOwner},
		agent.PromptRequest{Text: "delete it", Channel: "C1", Thread: "123.4"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	select {
	case <-rig.approved:
		close(asked)
	case <-time.After(10 * time.Second):
		t.Fatal("the gateway's approval gate was never asked")
	}

	sessionID, _ := manager.Lookup(key)
	cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = manager.Cancel(cancelCtx, sessionID)
	for range events {
	}

	select {
	case got := <-outcome:
		if got.allowed {
			t.Fatal("an abandoned approval was answered with consent")
		}
		if got.note == "" {
			t.Fatal("an abandoned approval was answered with no note, so the model is told nothing")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an approval outstanding when the turn ended was never answered")
	}
}

// An attachment's bytes cross as a side transfer, and the chunks are sent
// AHEAD of the event that references them. That ordering is the whole design:
// the gateway materialises an attachment from inside the link's read loop, so a
// deliverer that had to pull chunks arriving on that same loop would deadlock
// it.
func TestAnAttachmentCrossesAsASideTransfer(t *testing.T) {
	body := bytes.Repeat([]byte("murtaugh "), 40000) // ~360 KiB: several chunks
	script := newScriptedAgent(func(turn *scriptedTurn) {
		turn.emit(agent.Event{Type: agent.EventAttachment, Attachment: &agent.AttachmentEvent{
			Filename: "report.txt",
			Title:    "the report",
			Mimetype: "text/plain",
			Data:     body,
		}})
	})
	rig := dialLoopback(t, script)

	events, err := rig.sessions["default"].Prompt(context.Background(),
		agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"},
		agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4", UserID: nodeOwner},
		agent.PromptRequest{Text: "send me the report", Channel: "C1", Thread: "123.4"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}

	var got *agent.AttachmentEvent
	for ev := range events {
		if ev.Type == agent.EventError {
			t.Fatalf("the attachment failed to cross: %v", ev.Error)
		}
		if ev.Type == agent.EventAttachment {
			got = ev.Attachment
		}
	}
	if got == nil {
		t.Fatal("no attachment reached the gateway")
	}
	if got.Filename != "report.txt" || got.Title != "the report" {
		t.Fatalf("the attachment's metadata came through as %+v", got)
	}
	if got.Path == "" {
		t.Fatal("the attachment arrived with no byte source; the uploader has nothing to send")
	}
	landed, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatalf("read the delivered file: %v", err)
	}
	if !bytes.Equal(landed, body) {
		t.Fatalf("the delivered file is %d bytes, want %d", len(landed), len(body))
	}
}

// A background event — one a session emits with no turn open — crosses the link
// addressed by SESSION and reaches the gateway's background router.
//
// This is the feature split/06-liveness exists to render: without it a
// claude_code stretch that goes quiet on a node produces no notice at all, and
// because the gateway-side plumbing is complete the failure is silent on the
// side anyone would debug.
func TestABackgroundEventReachesTheGatewaysRouter(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(func(turn *scriptedTurn) {
		turn.emit(agent.Event{Type: agent.EventComplete, StopReason: "end_turn"})
	}))

	manager := rig.sessions["default"]
	key := agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"}
	events, err := manager.Prompt(context.Background(), key,
		agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4", UserID: nodeOwner},
		agent.PromptRequest{Text: "start something long", Channel: "C1", Thread: "123.4"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	for range events {
	}
	sessionID, ok := manager.Lookup(key)
	if !ok {
		t.Fatal("the manager did not record the node's session")
	}

	// The turn is over. This is what claude_code's emit does when a subagent
	// finishes afterwards: no active turn, so it goes to the node's sink.
	rig.background.Handle(sessionID, agent.Event{Type: agent.EventText, Text: "the subagent finished"})

	select {
	case got := <-rig.notices:
		if got.sessionID != sessionID {
			t.Fatalf("the notice was addressed to session %q, want %q", got.sessionID, sessionID)
		}
		if got.event.Type != agent.EventText || got.event.Text != "the subagent finished" {
			t.Fatalf("the background event crossed as %+v", got.event)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a background event never reached the gateway; a stretch that goes quiet on a node would produce no notice")
	}
}

// A background event addressed to a session must not be delivered as an
// approval request. There is no stream for an answer to come back on, so
// encoding one would register a correlation id nothing can resolve and park the
// backend goroutine that raised it.
func TestABackgroundApprovalIsAnsweredRatherThanSent(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}))

	decision := make(chan string, 1)
	rig.background.Handle("node-session-1", agent.Event{
		Type: agent.EventPermission,
		Permission: &agent.PermissionPrompt{
			Request:  agent.PermissionRequest{ToolKind: "terminal", ToolTitle: "rm -rf /"},
			Decision: decision,
		},
	})

	select {
	case answer := <-decision:
		if answer != "" {
			t.Fatalf("a background approval was answered with %q; nobody was asked, so it must resolve as dismissed", answer)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a background approval was neither answered nor refused; the backend goroutine that raised it is parked")
	}
	select {
	case got := <-rig.notices:
		t.Fatalf("an approval request was rendered as a background event: %+v", got.event)
	case <-time.After(200 * time.Millisecond):
	}
}

// A node that presents a credential the gateway does not know must be refused
// before the upgrade, with an HTTP status a dialler can print.
func TestAnUnknownCredentialIsRefusedBeforeTheUpgrade(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	host, err := nodehost.New(nodehost.Options{Tokens: &memTokens{records: map[string]config.NodeToken{}}, Logger: testLogger()})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	// Leading, so the 401 below is the credential's refusal and not leadership's.
	host.FollowLeader(elected{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = host.Serve(ctx, listener) }()

	minted, err := nodetoken.Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	_, err = nodesocket.Dial(ctx, "ws://"+listener.Addr().String(), nodesocket.DialOptions{Token: minted.Token})
	if err == nil {
		t.Fatal("an unknown credential was accepted")
	}
	if got := err.Error(); !contains(got, "401") {
		t.Fatalf("the refusal did not reach the node as a status it can print: %v", err)
	}
	if _, attached := host.Attached(); attached {
		t.Fatal("a rejected node was attached anyway")
	}
}

// Revoking a credential closes the connection it authenticated, which is what
// #170 says revocation means. It closes by selector, never by node: during a
// rotation a node holds two live credentials.
func TestRevokingACredentialClosesItsConnection(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}))
	if _, ok := rig.host.Attached(); !ok {
		t.Fatal("the node did not attach")
	}
	// The selector is the one the rig minted; the host holds it.
	if err := rig.host.CloseCredential(context.Background(), rig.selector); err != nil {
		t.Fatalf("close credential: %v", err)
	}
	waitFor(t, "the node to be dropped", func() bool {
		_, ok := rig.host.Attached()
		return !ok
	})
}

// The other half of that rule, and the half the overlap window exists for.
// During a rotation a node holds two live credentials: it has already moved to
// the new one and the old one is revoked behind it. Closing "every connection
// of node X" would drop the node in the middle of the seamless operation, so
// the match is on SELECTOR alone.
func TestRevokingADifferentCredentialLeavesTheConnectionUp(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(func(turn *scriptedTurn) {
		turn.emit(agent.Event{Type: agent.EventText, Text: "still here"})
	}))

	// The credential being retired: same node, different selector — which is
	// exactly the state a rotation's overlap window is.
	retired := mintInto(t, rig.store, "node-1")
	if retired.Selector == rig.selector {
		t.Fatal("the two credentials share a selector; the rig cannot pose a rotation")
	}
	if _, _, err := rig.store.Revoke(context.Background(), retired.Selector, time.Now()); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if err := rig.host.CloseCredential(context.Background(), retired.Selector); err != nil {
		t.Fatalf("close credential: %v", err)
	}

	if _, ok := rig.host.Attached(); !ok {
		t.Fatal("revoking the credential the node had already rotated OFF dropped the node")
	}
	// Attached is a flag; a turn is the proof the link still carries anything.
	events, err := rig.sessions["default"].Prompt(context.Background(),
		agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"},
		agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4", UserID: nodeOwner},
		agent.PromptRequest{Text: "are you there", Channel: "C1", Thread: "123.4"})
	if err != nil {
		t.Fatalf("the node stopped serving after an unrelated credential was revoked: %v", err)
	}
	if first := receiveEvent(t, events); first.Text != "still here" {
		t.Fatalf("the surviving connection answered with %+v", first)
	}
	for range events {
	}
}

// The node's agent is initialized at the HANDSHAKE, not lazily on the first
// turn. agent.SessionManager latches "initialized" on first success and never
// repeats it, so a node that attaches after the gateway's first turn would
// otherwise never be initialized at all — and the symptom is one conversation
// class working and another not, long after whatever change caused it.
func TestTheNodesAgentIsInitializedAtTheHandshake(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}))

	// Nothing has been prompted. The count can only come from the handshake.
	waitFor(t, "the node's agent to be initialized", func() bool {
		return rig.agent.initializes() == 1
	})
}

// And a node whose agent will not come up is not published. Publishing it would
// give every conversation an agent that fails on its first turn, with the
// failure surfacing to a user rather than to the operator who attached it.
func TestANodeWhoseAgentWillNotInitializeIsNotPublished(t *testing.T) {
	script := newScriptedAgent(func(*scriptedTurn) {})
	script.initErr = errors.New("the backend is not installed on this machine")

	rig := dialLoopback(t, script, withoutWaitingForAttach())

	// Give the handshake every chance to publish it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if nodeID, ok := rig.host.Attached(); ok {
			t.Fatalf("a node whose agent could not initialize was published as %q", nodeID)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := script.initializes(); n == 0 {
		t.Fatal("the handshake never tried to initialize the node's agent")
	}
}

// A second node JOINS the first. This is the behaviour #195 replaces: #193
// shipped one slot, where the newcomer evicted the incumbent and the gateway
// forgot a machine that was still connected and still willing to work.
func TestASecondNodeJoinsRatherThanEvicting(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}))
	if nodeID, _ := rig.host.Attached(); nodeID != "node-1" {
		t.Fatalf("the first node attached as %q", nodeID)
	}

	attachAnother(t, rig, "node-2")
	waitFor(t, "both nodes to be in the registry", func() bool {
		return len(rig.host.Nodes()) == 2
	})

	// The incumbent's connection is untouched. A registry that evicted it would
	// look identical from the newcomer's side and be wrong from the user's.
	select {
	case <-rig.nodeStopped:
		t.Fatal("the first node's connection was closed; a second node must join the registry, not replace the first")
	case <-time.After(200 * time.Millisecond):
	}

	nodes := rig.host.Nodes()
	if nodes[0].NodeID != "node-1" || nodes[1].NodeID != "node-2" {
		t.Fatalf("the registry lists %q and %q; it must be ordered so two gateways given one fleet choose alike", nodes[0].NodeID, nodes[1].NodeID)
	}
	// Identity comes from the credential each connection presented, never from
	// anything the node said — there is no node id on the wire at all.
	for _, node := range nodes {
		if node.UserID == "" || node.Selector == "" || node.AttachedAt.IsZero() {
			t.Fatalf("registry entry %+v is missing what the credential established", node)
		}
	}
}

// A conversation stays on the node that opened it, and the second node that
// attaches does not inherit it.
//
// With one slot this could not be posed: a second node evicted the first, so
// there was never another node for a conversation to leak onto. With a registry
// the first node is still connected and still serving, and routing every call to
// "whichever node is newest" hands the newcomer a session id it never minted —
// so the user's next message in a thread is answered by a machine with none of
// the conversation's history, working directory or files.
func TestAConversationStaysOnItsOwnNodeWhenASecondAttaches(t *testing.T) {
	first := newScriptedAgent(func(turn *scriptedTurn) {
		turn.emit(agent.Event{Type: agent.EventText, Text: "the first node"})
	})
	rig := dialLoopback(t, first)

	manager := rig.sessions["default"]
	key := agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"}
	meta := agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4", UserID: nodeOwner}
	drainPrompt(t, manager, key, meta, "turn one")

	// A second node joins and is now the most recently attached — the answer
	// every unbound lookup gives.
	second := attachScripted(t, rig, "node-2", newScriptedAgent(func(turn *scriptedTurn) {
		turn.emit(agent.Event{Type: agent.EventText, Text: "the second node"})
	}))

	if got := drainPrompt(t, manager, key, meta, "turn two"); got != "the first node" {
		t.Fatalf("the second turn of a live conversation was answered by %q; a node that attached mid-conversation took it over", got)
	}
	if n := second.agent.prompts(); n != 0 {
		t.Fatalf("the node that joined later was prompted %d times for a conversation it never opened", n)
	}
	if n := first.prompts(); n != 2 {
		t.Fatalf("the conversation's own node served %d of its 2 turns", n)
	}
}

// And a cancel reaches the node actually running the turn.
//
// This is the failure that costs more than a wrong answer. The gateway's /stop
// and its idle path both do `cancel(); for range events {}`; a cancel delivered
// to a node that holds no such session is answered as SUCCESS — remote cancel of
// an unknown session is idempotent by design — so the gateway reports the turn
// interrupted, the turn keeps running on the other node, and the drain blocks
// until the full idle timeout.
func TestACancelReachesTheNodeRunningTheTurnRatherThanTheNewestOne(t *testing.T) {
	started := make(chan struct{})
	script := newScriptedAgent(func(turn *scriptedTurn) {
		turn.emit(agent.Event{Type: agent.EventText, Text: "working"})
		close(started)
		<-turn.cancelled
		turn.emit(agent.Event{Type: agent.EventError, Error: context.Canceled})
	})
	rig := dialLoopback(t, script)

	manager := rig.sessions["default"]
	key := agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"}
	events, err := manager.Prompt(context.Background(), key,
		agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4", UserID: nodeOwner},
		agent.PromptRequest{Text: "long job", Channel: "C1", Thread: "123.4"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if first := receiveEvent(t, events); first.Text != "working" {
		t.Fatalf("first event was %+v", first)
	}
	<-started

	// The turn is in flight on the first node when the second one arrives.
	attachAnother(t, rig, "node-2")

	sessionID, ok := manager.Lookup(key)
	if !ok {
		t.Fatal("the manager did not record the node's session")
	}
	cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := manager.Cancel(cancelCtx, sessionID); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range events {
		}
	}()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn's channel never closed after a cancel; the cancel went to a node that was not running it, was answered as success, and the gateway would block for its full idle timeout")
	}
	if n := script.cancels(); n != 1 {
		t.Fatalf("the node running the turn was cancelled %d times, want 1", n)
	}
}

// A conversation whose node is gone MOVES, and the model that inherits it is
// told so — on the real path, end to end.
//
// This is the whole of #196's "recovers VISIBLY" and #170's "a conversation is
// never lost silently", and it is also the justification for not building #170
// Change G's card: the move is made visible by the model itself. The notice
// reaching the node is therefore not a detail of takeover.go — it is the
// feature. It travels through four seams that each look harmless alone: the
// binding answers ErrSessionGone, the session manager opens a fresh session,
// the election marks the takeover against that session, and the next prompt
// folds it into the user message. A test that called foldTakeover directly
// would assert the notice's SHAPE while every one of those seams could be
// deleted with the suite green.
func TestAConversationWhoseNodeIsGoneMovesAndTheModelIsToldInsideTheTurn(t *testing.T) {
	pins, err := configstore.OpenConversationPins(context.Background(),
		config.DatabaseConfig{Backend: config.BackendSQLite,
			SQLite: config.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")}}, "", "")
	if err != nil {
		t.Fatalf("open pins: %v", err)
	}
	t.Cleanup(func() { _ = pins.Close() })
	rec := &recordingJournal{}

	rig := dialLoopback(t, newScriptedAgent(func(turn *scriptedTurn) {
		turn.emit(agent.Event{Type: agent.EventText, Text: "the first node"})
	}), pinning(pins), journalling(rec))

	manager := rig.sessions["default"]
	key := agent.ConversationKey{TeamID: "T1", ChannelID: "C1", ThreadTS: "123.4"}
	meta := agent.SessionMetadata{TeamID: "T1", ChannelID: "C1", ThreadTS: "123.4", UserID: nodeOwner}
	drainPrompt(t, manager, key, meta, "turn one")

	successor := attachScripted(t, rig, "node-2", newScriptedAgent(func(turn *scriptedTurn) {
		turn.emit(agent.Event{Type: agent.EventText, Text: "the second node"})
	}))
	// The conversation's node goes: a revoked credential is a real drop, and it
	// is the shape a laptop's lid has.
	if err := rig.host.CloseCredential(context.Background(), rig.selector); err != nil {
		t.Fatalf("close credential: %v", err)
	}
	waitFor(t, "the conversation's node to go", func() bool {
		nodes := rig.host.Nodes()
		return len(nodes) == 1 && nodes[0].NodeID == "node-2"
	})

	// The user's next message in the SAME thread. It is served rather than
	// failed — the conversation is not lost with the machine.
	if got := drainPrompt(t, manager, key, meta, "what did we decide?"); got != "the second node" {
		t.Fatalf("the turn after the node left was answered by %q", got)
	}

	moved := successor.agent.lastPrompt()
	if !strings.Contains(moved.Text, "<conversation-takeover>") {
		t.Fatalf("the node that inherited the conversation was told nothing about the move; the model answers confidently about work it cannot see:\n%q", moved.Text)
	}
	if !strings.Contains(moved.Text, "node-1") {
		t.Fatalf("the notice does not say where the conversation came from:\n%q", moved.Text)
	}
	if !strings.HasSuffix(moved.Text, "what did we decide?") {
		t.Fatalf("the user's own words are not the tail of the message:\n%q", moved.Text)
	}

	// The pin was overwritten rather than bypassed: left naming the dead
	// machine, every later turn re-elects and lands somewhere new.
	pin, found, err := pins.Get(context.Background(),
		config.ConversationRef{TeamID: "T1", ChannelID: "C1", ThreadTS: "123.4"})
	if err != nil || !found {
		t.Fatalf("the moved conversation left no pin: found=%v err=%v", found, err)
	}
	if pin.NodeID != "node-2" {
		t.Fatalf("the stored pin still names %q", pin.NodeID)
	}

	// Journalled, because nothing about a node is announced — this record is
	// what somebody debugging "why did the agent forget" comes for.
	takeover := rec.find("takeover")
	if takeover.Kind != "delegation" {
		t.Fatal("the conversation moved machines and nothing was journalled; the only other trace is the model's own sentence, which is not queryable")
	}
	if takeover.Payload["previous_node_id"] != "node-1" || takeover.Payload["node_id"] != "node-2" {
		t.Fatalf("the record does not say what moved where: %+v", takeover.Payload)
	}

	// And the notice is consumed: a model told on every message that it has
	// just arrived and can see nothing behaves as though that were true.
	drainPrompt(t, manager, key, meta, "and the third turn")
	if strings.Contains(successor.agent.lastPrompt().Text, "conversation-takeover") {
		t.Fatalf("the notice was repeated on a later turn:\n%q", successor.agent.lastPrompt().Text)
	}
}

// A grant written in the gateway's configuration reaches an election, and it
// reaches it through the RUNTIME BUILDER.
//
// The grant tests next door call host.setAccess directly, so deleting that one
// line from the builder leaves them all green — and the cost of losing it is not
// subtle: Host.access stays the zero value for the process's life, GrantsOn
// always answers false, and every guest holding a grant is told "no runtime node
// of yours is connected" while the machine they were granted is sitting there
// connected and idle.
func TestAGrantInTheGatewaysConfigurationReachesAnElection(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(func(turn *scriptedTurn) {
		turn.emit(agent.Event{Type: agent.EventText, Text: "the granted node"})
	}))

	// A guest with no node of their own. Without the grant this is ErrNoFleet.
	const guest = "U-guest"
	key := agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"}
	meta := agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4", UserID: guest}
	if _, err := rig.sessions["default"].Prompt(context.Background(), key, meta,
		agent.PromptRequest{Text: "before the grant"}); !errors.Is(err, nodehost.ErrNoFleet) {
		t.Fatalf("a guest with no grant got %v, want ErrNoFleet", err)
	}

	// The gateway admin adds the grant and the configuration is reloaded, which
	// re-runs the runtime builder over the surviving node connection. This is
	// the only way a grant is ever added.
	granted := rig.reload(t, config.AccessConfig{NodeGrants: map[string][]string{"node-1": {guest}}})

	if got := drainPrompt(t, granted, key, meta, "after the grant"); got != "the granted node" {
		t.Fatalf("the granted guest was answered by %q", got)
	}
}

// drainPrompt runs one turn to completion and returns the prose it produced.
func drainPrompt(t *testing.T, manager *agent.SessionManager, key agent.ConversationKey, meta agent.SessionMetadata, text string) string {
	t.Helper()
	events, err := manager.Prompt(context.Background(), key, meta,
		agent.PromptRequest{Text: text, Channel: meta.ChannelID, Thread: meta.ThreadTS, User: meta.UserID})
	if err != nil {
		t.Fatalf("prompt %q: %v", text, err)
	}
	var prose string
	for ev := range events {
		if ev.Type == agent.EventText {
			prose += ev.Text
		}
		if ev.Type == agent.EventError {
			t.Fatalf("the turn failed: %v", ev.Error)
		}
	}
	return prose
}

// The SAME credential dialling back in does replace, and must: a node whose
// laptop slept is very often behind a half-dead socket the gateway has not
// noticed, and refusing the newcomer would lock it out until the gateway
// restarted. That is what cmd/murtaugh-runtime's jittered redial loop meets.
func TestARedialOnTheSameCredentialReplacesItsOwnConnection(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}))

	redial(t, rig)

	select {
	case <-rig.nodeStopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the previous connection was never closed; it would sit there believing it was still serving")
	}
	waitFor(t, "one entry for the one node", func() bool {
		nodes := rig.host.Nodes()
		return len(nodes) == 1 && nodes[0].NodeID == "node-1"
	})
}

// ONE machine on two live connections is one node.
//
// This is the rotation the overlap window exists for: the node has dialled back
// in on its new credential while the old connection is still up. The registry
// keeps both, because closing by selector is what makes revoking the old one
// safe — but enumeration collapses them, because a fleet list that named the
// same machine twice would round-robin a node against itself and call the result
// balance, and the symptom of that is a machine quietly taking twice its share
// of the work. The tie-break is the attach time, so the entry is the connection
// that would actually be used.
func TestOneNodeOnTwoConnectionsIsListedOnce(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}))
	first := rig.host.Nodes()
	if len(first) != 1 {
		t.Fatalf("the registry holds %d nodes before the rotation, want 1", len(first))
	}

	// The same NODE on a second credential — a rotation in progress, which is
	// the only state in which this can arise.
	rotated := attachScripted(t, rig, "node-1", newScriptedAgent(func(*scriptedTurn) {}))
	if rotated.selector == rig.selector {
		t.Fatal("the two credentials share a selector; the rig cannot pose a rotation")
	}
	// Both connections are genuinely live: the point is a collapse of two
	// entries, not the eviction of one.
	select {
	case <-rig.nodeStopped:
		t.Fatal("the rotation's second connection displaced the first; the overlap window exists so it does not")
	case <-rotated.stopped:
		t.Fatal("the rotation's second connection ended")
	case <-time.After(200 * time.Millisecond):
	}

	nodes := rig.host.Nodes()
	if len(nodes) != 1 {
		t.Fatalf("one machine on two live connections is listed %d times; delegation would round-robin it against itself", len(nodes))
	}
	if !nodes[0].AttachedAt.After(first[0].AttachedAt) {
		t.Fatalf("the entry is the OLDER connection (%s, was %s); the tie-break must name the one a turn would be sent to",
			nodes[0].AttachedAt, first[0].AttachedAt)
	}
	// And revoking the retired credential leaves the machine listed, which is
	// the whole reason the registry keeps both in the first place.
	if err := rig.host.CloseCredential(context.Background(), rig.selector); err != nil {
		t.Fatalf("close credential: %v", err)
	}
	waitFor(t, "the retired connection to close", func() bool {
		select {
		case <-rig.nodeStopped:
			return true
		default:
			return false
		}
	})
	if got := rig.host.Nodes(); len(got) != 1 || got[0].NodeID != "node-1" {
		t.Fatalf("after revoking the retired credential the registry holds %+v", got)
	}
}

// Revocation closes the connection on the revoked credential and leaves the
// rest of the fleet alone.
//
// It is deliberately NOT named "every connection on the credential": there can
// only ever be one, because insert removes any existing entry sharing a selector
// before publishing the newcomer. These two connections hold DIFFERENT
// credentials, which is the state that can actually arise, and the property
// worth pinning is that one node's revocation is not the fleet's.
func TestRevocationClosesTheRevokedConnectionAndLeavesTheFleet(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}))
	attachAnother(t, rig, "node-2")
	waitFor(t, "both nodes to be in the registry", func() bool {
		return len(rig.host.Nodes()) == 2
	})

	if err := rig.host.CloseCredential(context.Background(), rig.selector); err != nil {
		t.Fatalf("close credential: %v", err)
	}
	select {
	case <-rig.nodeStopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the revoked node kept serving")
	}
	waitFor(t, "only the other node to remain", func() bool {
		nodes := rig.host.Nodes()
		return len(nodes) == 1 && nodes[0].NodeID == "node-2"
	})
}

// redial brings the SAME node back after its connection dropped: the same
// agent, the same tool proxy and the same credential, over a NEW socket.
//
// It is what cmd/murtaugh-runtime's redial loop does about a second after a
// laptop wakes up, and it is the only way a test can observe whether anything on
// either side repeats a call that was in flight when the previous connection
// died — with one connection there is no path by which a repeat could arrive.
func redial(t *testing.T, rig *loopback) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	conn, err := nodesocket.Dial(ctx, "ws://"+rig.addr, nodesocket.DialOptions{Token: rig.token})
	if err != nil {
		t.Fatalf("redial: %v", err)
	}
	served := make(chan error, 1)
	go func() {
		served <- nodeserve.Serve(ctx, conn, rig.agent, nodeserve.Options{
			Logger:     testLogger(),
			Gate:       rig.gate,
			Background: rig.background,
			Tools:      rig.proxy,
			// The same advertiser: a reconnect re-advertises from scratch,
			// which is what makes dropping a push while unbound harmless.
			Advertise:   rig.claim,
			WindowBytes: nodesocket.DefaultWindowBytes,
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-served
	})
	waitFor(t, "the node to attach again", func() bool {
		_, ok := rig.host.Attached()
		return ok
	})
}

// peer is a SECOND node on the same gateway: its own credential, its own
// connection, and its own agent the test can question. The rig's own node is the
// first; this is how a test poses a fleet.
type peer struct {
	nodeID   string
	selector string
	agent    *scriptedAgent
	stopped  chan struct{}
}

// attachAnother dials a second node into the same gateway.
func attachAnother(t *testing.T, rig *loopback, nodeID string) *peer {
	t.Helper()
	return attachScripted(t, rig, nodeID, newScriptedAgent(func(*scriptedTurn) {}))
}

// attachScripted is attachAnother with an agent the caller can observe — which
// is what it takes to tell "the turn went to the right node" from "the turn
// went somewhere and came back".
func attachScripted(t *testing.T, rig *loopback, nodeID string, script *scriptedAgent) *peer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	minted := mintInto(t, rig.store, nodeID)
	conn, err := nodesocket.Dial(ctx, "ws://"+rig.addr, nodesocket.DialOptions{Token: minted.Token})
	if err != nil {
		t.Fatalf("dial the second node: %v", err)
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = nodeserve.Serve(ctx, conn, script, nodeserve.Options{
			Logger:      testLogger(),
			WindowBytes: nodesocket.DefaultWindowBytes,
		})
	}()
	t.Cleanup(func() {
		cancel()
		<-stopped
	})
	joined := &peer{nodeID: nodeID, selector: minted.Selector, agent: script, stopped: stopped}
	waitFor(t, "the second node to attach", func() bool {
		for _, node := range rig.host.Nodes() {
			if node.NodeID == nodeID {
				return true
			}
		}
		return false
	})
	return joined
}

// ---- fakes -----------------------------------------------------------------

// scriptedAgent is an agent.Client whose turns are a Go function. It is the one
// thing in these tests that is not production code, because what is under test
// is the hop and not the model.
type scriptedAgent struct {
	script func(*scriptedTurn)
	gate   *nodeserve.ToolGate
	// tools is the node-side registry a real backend's toolset would be
	// resolved from. A scripted turn reaches into it the way a model's tool call
	// would.
	tools *tools.Registry
	// initErr makes the agent refuse to come up, which is a real state — a
	// backend binary that is not installed on the node's machine.
	initErr error

	mu        sync.Mutex
	sessions  map[string]*scriptedTurn
	last      agent.PromptRequest
	prompted  int
	cancelled int
	initCalls int
	// The two moments a real backend latches its toolset, recorded as the names
	// visible in the node's registry at each. native resolves inside Initialize;
	// an acp/claude_code agent's aggregator resolves when its first session is
	// registered, which is inside NewSession. Both must already see the
	// gateway's tools or that backend is tool-less for the life of the process.
	atInitialize []string
	atNewSession []string
}

type scriptedTurn struct {
	ctx       context.Context
	gate      *nodeserve.ToolGate
	tools     *tools.Registry
	events    chan agent.Event
	cancelled chan struct{}
	once      sync.Once
}

// invoke calls a tool out of the node's registry, which is what a backend does
// with the toolset toolset.Resolve handed it.
func (t *scriptedTurn) invoke(name string, args map[string]any) (any, error) {
	if t.tools == nil {
		return nil, errors.New("this rig was built without a tool channel")
	}
	tool, ok := t.tools.Get(name)
	if !ok {
		return nil, fmt.Errorf("no tool named %q reached the node", name)
	}
	return tool.Invoke(t.ctx, args)
}

func newScriptedAgent(script func(*scriptedTurn)) *scriptedAgent {
	return &scriptedAgent{script: script, sessions: make(map[string]*scriptedTurn)}
}

func (a *scriptedAgent) Initialize(context.Context) error {
	a.mu.Lock()
	a.initCalls++
	a.atInitialize = registryNames(a.tools)
	err := a.initErr
	a.mu.Unlock()
	return err
}

func (a *scriptedAgent) NewSession(_ context.Context, _ agent.SessionMetadata) (agent.Session, error) {
	a.mu.Lock()
	a.atNewSession = registryNames(a.tools)
	a.mu.Unlock()
	return agent.Session{ID: "node-session-1"}, nil
}

// registryNames is what a backend resolving its toolset out of the node's
// registry would see at that instant.
func registryNames(reg *tools.Registry) []string {
	if reg == nil {
		return nil
	}
	var names []string
	for _, t := range reg.All() {
		names = append(names, t.Name())
	}
	return names
}

func (a *scriptedAgent) Prompt(ctx context.Context, sessionID string, req agent.PromptRequest) (<-chan agent.Event, error) {
	turn := &scriptedTurn{
		ctx:       ctx,
		gate:      a.gate,
		tools:     a.tools,
		events:    make(chan agent.Event, 8),
		cancelled: make(chan struct{}),
	}
	a.mu.Lock()
	a.last = req
	a.prompted++
	a.sessions[sessionID] = turn
	a.mu.Unlock()

	go func() {
		defer close(turn.events)
		a.script(turn)
	}()
	return turn.events, nil
}

func (a *scriptedAgent) Cancel(_ context.Context, sessionID string) error {
	a.mu.Lock()
	turn := a.sessions[sessionID]
	a.cancelled++
	a.mu.Unlock()
	if turn == nil {
		// Idempotent, the way acp.Client.Cancel is for an unknown session.
		return nil
	}
	turn.once.Do(func() { close(turn.cancelled) })
	return nil
}

func (a *scriptedAgent) Close() error { return nil }

func (a *scriptedAgent) lastPrompt() agent.PromptRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

// prompts is how many turns this node's agent was asked to run. It is the only
// way to tell which of two connected nodes a turn actually reached.
func (a *scriptedAgent) prompts() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.prompted
}

func (a *scriptedAgent) cancels() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cancelled
}

func (a *scriptedAgent) initializes() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.initCalls
}

func (a *scriptedAgent) toolsAtInitialize() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.atInitialize...)
}

func (a *scriptedAgent) toolsAtNewSession() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.atNewSession...)
}

func (t *scriptedTurn) emit(ev agent.Event) {
	select {
	case t.events <- ev:
	case <-t.ctx.Done():
	}
}

type approverFunc func(context.Context, string, string) (bool, string)

func (f approverFunc) Approve(ctx context.Context, toolName, summary string) (bool, string) {
	return f(ctx, toolName, summary)
}

// memTokens is the credential store in memory. The SQL implementations have
// their own tests; what these need is a store that answers.
type memTokens struct {
	mu      sync.Mutex
	records map[string]config.NodeToken
}

func (m *memTokens) Put(_ context.Context, token config.NodeToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.records[token.Selector]; exists {
		return fmt.Errorf("selector %s already exists", token.Selector)
	}
	m.records[token.Selector] = token
	return nil
}

func (m *memTokens) BySelector(_ context.Context, selector string) (config.NodeToken, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[selector]
	return record, ok, nil
}

func (m *memTokens) List(_ context.Context, nodeID string) ([]config.NodeToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]config.NodeToken, 0, len(m.records))
	for _, record := range m.records {
		if nodeID == "" || record.NodeID == nodeID {
			out = append(out, record)
		}
	}
	return out, nil
}

func (m *memTokens) Revoke(_ context.Context, selector string, at time.Time) (config.NodeToken, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[selector]
	if !ok {
		return config.NodeToken{}, false, nil
	}
	if record.RevokedAt.IsZero() {
		record.RevokedAt = at
	}
	m.records[selector] = record
	return record, true, nil
}

func (m *memTokens) Close() error { return nil }

// ---- helpers ---------------------------------------------------------------

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func receiveEvent(t *testing.T, events <-chan agent.Event) agent.Event {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("the turn's channel closed with no events")
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an event")
		return agent.Event{}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// TestARealTurnIsDelegatedAndPinned is delegation on the real path: a real
// socket, a real handshake, a real session manager, and a pin written to a real
// SQLite store.
//
// The unit tests next door prove the algorithm. This proves it is WIRED — that
// the conversation key reaches the broker at all, which it can only do by
// travelling on the context the session manager sets, and that the node id in
// the pin is the one the credential resolved to rather than anything the node
// said about itself.
func TestARealTurnIsDelegatedAndPinned(t *testing.T) {
	pins, err := configstore.OpenConversationPins(context.Background(),
		config.DatabaseConfig{Backend: config.BackendSQLite,
			SQLite: config.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")}}, "", "")
	if err != nil {
		t.Fatalf("open pins: %v", err)
	}
	t.Cleanup(func() { _ = pins.Close() })

	script := newScriptedAgent(func(turn *scriptedTurn) {
		turn.emit(agent.Event{Type: agent.EventText, Text: "ready."})
		turn.emit(agent.Event{Type: agent.EventComplete, StopReason: "end_turn"})
	})
	rig := dialLoopback(t, script, pinning(pins),
		claiming(agentwire.Advertisement{
			Profiles: []string{"default"},
			Claims:   []agentwire.AssignmentClaim{{Match: "nc-*", Profile: "default"}},
		}))

	key := agent.ConversationKey{TeamID: "T1", ChannelID: "C1", ThreadTS: "123.4", DM: false}
	events, err := rig.sessions["default"].Prompt(context.Background(), key,
		agent.SessionMetadata{TeamID: "T1", ChannelID: "C1", ChannelName: "nc-releases",
			ThreadTS: "123.4", UserID: nodeOwner},
		agent.PromptRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	for ev := range events {
		if ev.Type == agent.EventError {
			t.Fatalf("the turn failed: %v", ev.Error)
		}
	}

	pin, found, err := pins.Get(context.Background(),
		config.ConversationRef{TeamID: "T1", ChannelID: "C1", ThreadTS: "123.4"})
	if err != nil || !found {
		t.Fatalf("the served turn left no pin: found=%v err=%v", found, err)
	}
	if pin.NodeID != "node-1" {
		t.Fatalf("the pin names %q; the credential resolved to node-1", pin.NodeID)
	}
	if pin.UserID != nodeOwner {
		t.Fatalf("the pin records %q as the electing user, want %q", pin.UserID, nodeOwner)
	}
	// The prompt the node actually received must be the user's, unadorned: this
	// conversation was never anywhere else, so there is nothing to announce.
	if got := script.lastPrompt(); got.Text != "hello" {
		t.Fatalf("an ordinary turn carried something extra: %q", got.Text)
	}
}
