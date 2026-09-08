package nodehost_test

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/tools"
	"github.com/miere/murtaugh/internal/tools/ping"
	versiontool "github.com/miere/murtaugh/internal/tools/version"
)

// This file is #194's verification, over the same real WebSocket the rest of
// this package's loopback tests use: an agent on a node calling a Murtaugh tool
// that runs on the gateway, the reconnect policy, and the partition.
//
// Everything below the fakes is production code: the real Host, the real remote
// client, the real nodeserve.Server, the real ToolProxy and the real
// toolset.Reach table. The fakes are the agent (a scripted client — a language
// model is not what is under test) and a handful of tools standing in for
// registry entries whose real versions need a Slack token or a config store.

// 1. An agent on a loopback runtime calling a Murtaugh tool.
//
// The whole point of the item: without this an acp or claude_code agent on a
// node reaches nothing, because the aggregator it speaks MCP to is on the other
// side of the network.
func TestANodesAgentCallsAMurtaughToolOnTheGateway(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(ping.New())
	registry.Register(versiontool.New("1.2.3"))

	answered := make(chan any, 2)
	script := newScriptedAgent(func(turn *scriptedTurn) {
		pong, err := turn.invoke("ping", nil)
		if err != nil {
			t.Errorf("ping over the tool channel: %v", err)
		}
		answered <- pong
		build, err := turn.invoke("version", nil)
		if err != nil {
			t.Errorf("version over the tool channel: %v", err)
		}
		answered <- build
		turn.emit(agent.Event{Type: agent.EventComplete, StopReason: "end_turn"})
	})
	rig := dialLoopback(t, script, withTools(registry))

	events, err := rig.sessions["default"].Prompt(context.Background(),
		agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"},
		agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4"},
		agent.PromptRequest{Text: "are you there", Channel: "C1", Thread: "123.4", User: "U9"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	for range events {
	}

	if got := receiveAny(t, answered); got != "pong" {
		t.Fatalf("ping came back as %#v", got)
	}
	// A struct result crosses as the JSON both in-process frontends would have
	// rendered, so a proxied call and a local one read identically to a model.
	if got := receiveAny(t, answered); got != `{"version":"1.2.3"}` {
		t.Fatalf("version came back as %#v", got)
	}
}

// The descriptor has to carry more than a name. A tool that publishes under an
// MCP alias must keep it: `ask` publishes as AskUserQuestion so a Claude Code
// agent reaches for it by reflex, and a proxy that dropped the alias would not
// fail — the agent would simply stop asking, with nothing saying why.
func TestAProxiedToolKeepsItsPublishedNameAndSchema(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(&aliasedTool{})

	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}), withTools(registry))

	proxied, ok := rig.proxy.Registry().Get("ask")
	if !ok {
		t.Fatal("the node's registry has no proxy for `ask`")
	}
	namer, ok := proxied.(interface{ MCPName() string })
	if !ok {
		t.Fatal("the proxy does not implement MCPNamer, so `ask` republishes as `ask` and a Claude Code agent never reaches for it")
	}
	if namer.MCPName() != "AskUserQuestion" {
		t.Fatalf("the published name crossed as %q", namer.MCPName())
	}
	if proxied.Description() != "Ask the user a question." {
		t.Fatalf("the description crossed as %q", proxied.Description())
	}
	schema := proxied.InputSchema()
	if schema == nil || schema.Properties["question"] == nil {
		t.Fatalf("the input schema did not survive the hop: %#v", schema)
	}
}

// 2. The reconnect policy. There is no idempotency information anywhere in the
// tool surface, so an in-flight call must FAIL when the link dies — never be
// repeated, and never left hanging — and the failure has to be legible enough
// that a model does not retry a side-effecting tool on its own initiative.
//
// The node genuinely redials at the end, because "is not repeated" is not an
// assertion a single connection can carry: with nothing to repeat it ON, the
// counter could only ever read 1. The second connection is the one a laptop
// makes when its lid opens, and it is the connection a retry would arrive over.
func TestAnInFlightToolCallFailsWhenTheLinkDropsAndIsNotRepeatedOnTheNextConnection(t *testing.T) {
	blocking := &blockingTool{released: make(chan struct{})}
	registry := tools.NewRegistry()
	registry.Register(blocking)
	registry.Register(versiontool.New("1.2.3"))

	calling := make(chan struct{})
	outcome := make(chan any, 1)
	script := newScriptedAgent(func(turn *scriptedTurn) {
		close(calling)
		result, err := turn.invoke("ping", nil)
		if err != nil {
			t.Errorf("a dropped call came back as an error rather than a note: %v", err)
		}
		outcome <- result
	})
	rig := dialLoopback(t, script, withTools(registry))

	events, err := rig.sessions["default"].Prompt(context.Background(),
		agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"},
		agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4"},
		agent.PromptRequest{Text: "do the thing", Channel: "C1", Thread: "123.4"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	<-calling
	// The gateway is now inside the tool's Invoke and will not return.
	waitFor(t, "the gateway to start running the tool", func() bool { return blocking.started() == 1 })

	// A real drop rather than a process teardown: revoking the credential closes
	// the live connection, which is what #170 says revocation means and is the
	// same shape as a laptop closing its lid.
	if err := rig.host.CloseCredential(context.Background(), rig.selector); err != nil {
		t.Fatalf("close credential: %v", err)
	}
	for range events {
	}

	var note string
	select {
	case got := <-outcome:
		note, _ = got.(string)
	case <-time.After(10 * time.Second):
		t.Fatal("a tool call in flight when the link dropped never returned; the agent is parked forever")
	}
	// The clauses are named rather than asserted as one opaque string, so
	// weakening the wording fails on the clause that was weakened. This is the
	// backstop being checked, not the guarantee: by the time the note arrives the
	// scripted turn's context is already cancelled — nodeserve's shutdown ends
	// every turn before it fails the calls — and a real agent loop would abort
	// rather than read it. What the note protects is the caller whose context is
	// not a turn's, which is why it still has to say the right thing.
	for _, clause := range []string{"dropped", "may or may not have taken effect", "Do not retry"} {
		if !strings.Contains(note, clause) {
			t.Fatalf("the failure note omits %q, so a model may retry a side-effecting call: %q", clause, note)
		}
	}

	// The gateway must not still be executing it. This is the known gap #170
	// names: nothing cancels an in-flight tool handler when its connection dies,
	// and with a ten-minute approval timeout a drop mid-approval can fire a side
	// effect for an agent that no longer exists. On this path the context is
	// ours end to end, so it is closed rather than documented.
	waitFor(t, "the gateway to cancel the tool call it was running for a node that left", func() bool {
		return blocking.cancelled()
	})

	// And the call is not silently repeated by anything on either side. The node
	// dials back in — a fresh nodelink with its own epoch, a fresh handshake —
	// which is the only state in which a repeat could actually be delivered.
	blocking.release()
	redial(t, rig)
	// The new connection is live and carrying tool calls, so "nothing arrived"
	// below is a fact about the link rather than about a socket nobody is using.
	waitFor(t, "the tool channel to answer over the new connection", func() bool {
		build, err := rig.proxy.Call(context.Background(), "version", nil)
		return err == nil && build == `{"version":"1.2.3"}`
	})
	if n := blocking.started(); n != 1 {
		t.Fatalf("the tool was entered %d times for one call; a retry of a side-effecting tool is exactly what the policy forbids", n)
	}
}

// The gateway's tools have to be in the node's registry before EITHER of the
// two moments a backend latches its toolset, because both latches are permanent.
//
// native resolves inside its own Initialize; an acp/claude_code agent's
// aggregator resolves when its first session is registered, inside NewSession.
// The node's handshake therefore fetches the tool surface before it initializes
// the agent — and this pins that ordering from the agent's own point of view,
// which is the only vantage point from which the acp/claude_code failure is
// visible at all. A registry filled after either latch is a registry that
// backend never consults again: the agent is not degraded, it is tool-less for
// the life of the process, and the symptom is a model that quietly stops
// reaching for things.
func TestTheGatewaysToolsAreVisibleAtBothMomentsABackendLatchesItsToolset(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(ping.New())

	script := newScriptedAgent(func(turn *scriptedTurn) {
		turn.emit(agent.Event{Type: agent.EventComplete, StopReason: "end_turn"})
	})
	rig := dialLoopback(t, script, withTools(registry))

	// Initialize ran at the handshake; nothing has been prompted yet.
	if got := script.toolsAtInitialize(); !slices.Contains(got, "ping") {
		t.Fatalf("the node's agent saw %v when it initialized, so a native backend latches an empty toolset", got)
	}

	events, err := rig.sessions["default"].Prompt(context.Background(),
		agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"},
		agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4"},
		agent.PromptRequest{Text: "hello", Channel: "C1", Thread: "123.4"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	for range events {
	}
	if got := script.toolsAtNewSession(); !slices.Contains(got, "ping") {
		t.Fatalf("the node's agent saw %v when its first session opened, so an acp or claude_code aggregator serves nothing", got)
	}
}

// A proxied call carries the turn's Slack location, put back on the context
// gateway-side from the stream id the call names.
//
// This is the half of "two things a proxied call must carry" that is LIVE for
// the shipped partition: `ask` and `present_plan` both read
// agent.TurnLocationFromContext and refuse without it, so two of the four
// node-reachable tools break outright — and by the same argument the approval
// gate short-circuits to ALLOWED with no card, which fails open rather than
// shut. Nothing about the call looks wrong from the node's end, which is why it
// is pinned here rather than trusted.
func TestAProxiedCallCarriesTheTurnsSlackLocation(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(&locationTool{})

	located := make(chan any, 1)
	script := newScriptedAgent(func(turn *scriptedTurn) {
		where, err := turn.invoke("ask", nil)
		if err != nil {
			t.Errorf("ask over the tool channel: %v", err)
		}
		located <- where
		turn.emit(agent.Event{Type: agent.EventComplete, StopReason: "end_turn"})
	})
	rig := dialLoopback(t, script, withTools(registry))

	events, err := rig.sessions["default"].Prompt(context.Background(),
		agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"},
		agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4"},
		agent.PromptRequest{Text: "ask me something", Channel: "C1", Thread: "123.4", User: "U9"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	for range events {
	}

	if got := receiveAny(t, located); got != "C1/123.4" {
		t.Fatalf("the tool ran with location %#v; `ask` and `present_plan` refuse without one and the approval gate silently allows", got)
	}
}

// And it resolves that location against the CONNECTION that made the call, not
// against whichever node is newest.
//
// Stream ids are minted per client, so two connected nodes hold the same ids for
// different turns. Looking one up on the wrong node does not error — it finds
// nothing, and finding nothing here leaves the context bare, which is what
// ungates the call: the approval gate short-circuits to ALLOWED with no card,
// and `ask` and `present_plan` stop being interactive. The failure is invisible
// from the node's end and from the gateway's logs alike, which is why the fix
// item 9 made is pinned rather than trusted.
func TestAToolCallResolvesItsTurnOnTheConnectionThatMadeIt(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(&locationTool{})

	joined := make(chan struct{})
	located := make(chan any, 1)
	script := newScriptedAgent(func(turn *scriptedTurn) {
		// The tool call is made only once a second node is attached and is the
		// most recent — the answer any unbound lookup would give.
		<-joined
		where, err := turn.invoke("ask", nil)
		if err != nil {
			t.Errorf("ask over the tool channel: %v", err)
		}
		located <- where
		turn.emit(agent.Event{Type: agent.EventComplete, StopReason: "end_turn"})
	})
	rig := dialLoopback(t, script, withTools(registry))

	events, err := rig.sessions["default"].Prompt(context.Background(),
		agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"},
		agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4"},
		agent.PromptRequest{Text: "ask me something", Channel: "C1", Thread: "123.4", User: "U9"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	attachAnother(t, rig, "node-2")
	close(joined)
	for range events {
	}

	if got := receiveAny(t, located); got != "C1/123.4" {
		t.Fatalf("the tool ran with location %#v; the calling node's stream was resolved against another node's turns, which ungates the approval card", got)
	}
}

// The approval gate runs on the GATEWAY, once, for a proxied call — and a denial
// comes back as the call's result rather than as a fault.
//
// Nothing in the shipped partition triggers it yet: none of `ping`, `version`,
// `ask` or `present_plan` implements tools.ApprovalClassifier, so the gate
// cannot deny anything today. It is pinned anyway with a purpose-built gated
// tool, because this is the one place a human stands between a node's model and
// a gateway credential, the partition is designed to widen, and a regression
// here would ungate every proxied call with nothing to notice it. Replacing the
// gate call with a hardcoded "not denied" passed the whole scoped suite before
// this test existed.
func TestTheGatewayAsksItsHumanBeforeRunningAGatedToolForANode(t *testing.T) {
	gated := &gatedTool{}
	registry := tools.NewRegistry()
	registry.Register(gated)

	results := make(chan any, 2)
	script := newScriptedAgent(func(turn *scriptedTurn) {
		for i := 0; i < 2; i++ {
			out, err := turn.invoke("present_plan", map[string]any{"step": "delete everything"})
			if err != nil {
				t.Errorf("present_plan over the tool channel: %v", err)
			}
			results <- out
		}
		turn.emit(agent.Event{Type: agent.EventComplete, StopReason: "end_turn"})
	})
	rig := dialLoopback(t, script, withTools(registry))

	events, err := rig.sessions["default"].Prompt(context.Background(),
		agent.ConversationKey{ChannelID: "C1", ThreadTS: "123.4"},
		agent.SessionMetadata{ChannelID: "C1", ThreadTS: "123.4"},
		agent.PromptRequest{Text: "do it", Channel: "C1", Thread: "123.4", User: "U9"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}

	// The gateway's human is asked with the tool's own per-call summary — the
	// classification depends on the ARGUMENTS, so only the side holding the tool
	// can make it.
	ask := receiveApproval(t, rig)
	if ask.tool != "present_plan" || ask.summary != "present_plan: delete everything" {
		t.Fatalf("the gate was asked about %+v, so the tool's own per-call classification did not reach the human", ask)
	}
	ask.answer <- approvalAnswer{allowed: false, note: "Denied by the user. The action was not run."}

	if got := receiveAny(t, results); got != "Denied by the user. The action was not run." {
		t.Fatalf("a denial reached the model as %#v; it must arrive as the call's result, not as a fault", got)
	}
	if n := gated.runs(); n != 0 {
		t.Fatalf("a denied tool ran %d times on the gateway", n)
	}

	// And an allowed call runs, so the test cannot pass by refusing everything.
	allow := receiveApproval(t, rig)
	allow.answer <- approvalAnswer{allowed: true}
	if got := receiveAny(t, results); got != "planned" {
		t.Fatalf("an approved call came back as %#v", got)
	}
	if n := gated.runs(); n != 1 {
		t.Fatalf("an approved tool ran %d times", n)
	}
	for range events {
	}
}

// receiveApproval takes the next question the gateway's human was asked.
func receiveApproval(t *testing.T, rig *loopback) approval {
	t.Helper()
	select {
	case ask := <-rig.approved:
		return ask
	case <-time.After(10 * time.Second):
		t.Fatal("the gateway's approval gate was never asked, so a node's model reached a gateway credential unprompted")
		return approval{}
	}
}

// 3. The partition. A node asking for a tool it may not have is refused by the
// GATEWAY, not filtered by the node — a node admin owns their node's config and
// could list any family they liked in an agent's `tools:`, so the node is not a
// place this decision can live.
func TestTheGatewayRefusesAToolANodeMayNotReach(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(ping.New())
	registry.Register(&namedTool{name: "slack.send-msg"})
	registry.Register(&namedTool{name: "node.token.mint"})
	registry.Register(&namedTool{name: "cfg.agent.create"})

	script := newScriptedAgent(func(*scriptedTurn) {})
	rig := dialLoopback(t, script, withTools(registry))

	// The node is never even told the names.
	published := rig.proxy.Registry()
	for _, forbidden := range []string{"slack.send-msg", "node.token.mint", "cfg.agent.create"} {
		if _, ok := published.Get(forbidden); ok {
			t.Fatalf("%s was published to the node; its credentials are the gateway's", forbidden)
		}
	}
	if _, ok := published.Get("ping"); !ok {
		t.Fatal("ping did not cross, so this test would pass with a channel that carries nothing")
	}

	// And asking by name anyway is refused, which is the half that matters: the
	// node-side list is a convenience, the gateway-side check is the boundary.
	host := rig.host
	if _, ok := host.Attached(); !ok {
		t.Fatal("the node is not attached")
	}
	err := callByName(t, rig, "slack.send-msg")
	if err == nil {
		t.Fatal("the gateway ran a gateway-only tool because a node asked for it by name")
	}
	if !strings.Contains(err.Error(), "does not serve") {
		t.Fatalf("the refusal did not say what it was: %v", err)
	}
}

// callByName asks the gateway for a tool by name, bypassing the node's own
// registry — which deliberately has no entry for a forbidden tool, so there is
// nothing there to Invoke. This is the shape of a node that decided to ask
// anyway.
func callByName(t *testing.T, rig *loopback, name string) error {
	t.Helper()
	_, err := rig.proxy.Call(context.Background(), name, nil)
	return err
}

// ---- fakes -----------------------------------------------------------------

// namedTool is a registry entry that exists only to be classified. Invoking it
// is a test failure: nothing gateway-only may ever run for a node.
type namedTool struct{ name string }

func (t *namedTool) Name() string                    { return t.name }
func (t *namedTool) Description() string             { return "a stand-in for a credential-bearing tool" }
func (t *namedTool) InputSchema() *jsonschema.Schema { return nil }
func (t *namedTool) Invoke(context.Context, map[string]any) (any, error) {
	panic("a gateway-only tool was invoked for a node")
}

// aliasedTool stands in for `ask`: a tool whose registry key and published name
// differ, and which carries a real input schema.
type aliasedTool struct{}

func (t *aliasedTool) Name() string        { return "ask" }
func (t *aliasedTool) MCPName() string     { return "AskUserQuestion" }
func (t *aliasedTool) Description() string { return "Ask the user a question." }
func (t *aliasedTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:       "object",
		Properties: map[string]*jsonschema.Schema{"question": {Type: "string"}},
		Required:   []string{"question"},
	}
}

func (t *aliasedTool) Invoke(context.Context, map[string]any) (any, error) { return "asked", nil }

// locationTool stands in for `ask` and `present_plan`: a tool that does nothing
// but report the turn location it was invoked with. The two real ones return a
// hard error without it (ask.go, plan.go), so this reports rather than fails —
// the test wants to see WHAT crossed, not merely that something did.
type locationTool struct{}

func (t *locationTool) Name() string                    { return "ask" }
func (t *locationTool) Description() string             { return "reports the turn location it ran with" }
func (t *locationTool) InputSchema() *jsonschema.Schema { return nil }

func (t *locationTool) Invoke(ctx context.Context, _ map[string]any) (any, error) {
	location, ok := agent.TurnLocationFromContext(ctx)
	if !ok {
		return "no location", nil
	}
	return location.ChannelID + "/" + location.ThreadTS, nil
}

// gatedTool is a node-reachable tool that requires approval — which nothing in
// the shipped partition does yet. It exists so the gate can be exercised: it
// classifies per call from the arguments and summarises itself, which are the
// two optional interfaces the gateway reads.
type gatedTool struct{ entered atomic.Int64 }

func (t *gatedTool) Name() string                    { return "present_plan" }
func (t *gatedTool) Description() string             { return "a stand-in for a tool that must be approved" }
func (t *gatedTool) InputSchema() *jsonschema.Schema { return nil }

func (t *gatedTool) RequiresApproval(map[string]any) bool { return true }

func (t *gatedTool) ApprovalSummary(args map[string]any) string {
	step, _ := args["step"].(string)
	return "present_plan: " + step
}

func (t *gatedTool) Invoke(context.Context, map[string]any) (any, error) {
	t.entered.Add(1)
	return "planned", nil
}

func (t *gatedTool) runs() int { return int(t.entered.Load()) }

// blockingTool is registered as `ping` because the partition classifies by
// family name and `ping` is node-reachable. It blocks inside Invoke so a call
// can be in flight when the link dies, and records whether its context was
// cancelled — which is how the gateway proves it does not keep running a tool
// for an agent that has gone.
type blockingTool struct {
	entered  atomic.Int64
	released chan struct{}
	sawStop  atomic.Bool
	once     atomic.Bool
}

func (t *blockingTool) Name() string                    { return "ping" }
func (t *blockingTool) Description() string             { return "blocks until released" }
func (t *blockingTool) InputSchema() *jsonschema.Schema { return nil }

func (t *blockingTool) Invoke(ctx context.Context, _ map[string]any) (any, error) {
	t.entered.Add(1)
	select {
	case <-ctx.Done():
		t.sawStop.Store(true)
		return nil, ctx.Err()
	case <-t.released:
		return "pong", nil
	}
}

func (t *blockingTool) started() int    { return int(t.entered.Load()) }
func (t *blockingTool) cancelled() bool { return t.sawStop.Load() }

func (t *blockingTool) release() {
	if t.once.CompareAndSwap(false, true) {
		close(t.released)
	}
}

func receiveAny(t *testing.T, ch chan any) any {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a tool result")
		return nil
	}
}
