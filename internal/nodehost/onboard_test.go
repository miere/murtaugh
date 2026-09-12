package nodehost_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/journal"
	"github.com/miere/murtaugh/internal/nodehost"
)

type noticed struct {
	mu    sync.Mutex
	nodes []nodehost.Node
}

func (n *noticed) record(_ context.Context, node nodehost.Node) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.nodes = append(n.nodes, node)
}

func (n *noticed) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.nodes)
}

func (n *noticed) first() nodehost.Node {
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.nodes) == 0 {
		return nodehost.Node{}
	}
	return n.nodes[0]
}

func TestAZeroProfileNodeTriggersOnboardingOfItsOwner(t *testing.T) {
	seen := &noticed{}
	rec := &recordingJournal{}
	dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}),
		journalling(rec), onboarding(nil, seen.record))

	waitFor(t, "the unconfigured node to be noticed", func() bool { return seen.count() > 0 })
	node := seen.first()
	if node.UserID != nodeOwner {
		t.Errorf("the trigger names user %q, want %q; the owner comes from the credential", node.UserID, nodeOwner)
	}
	if node.NodeID == "" {
		t.Error("the trigger names no node; a completed form has nowhere to be sent")
	}

	waitFor(t, "the unconfigured arrival to be journalled", func() bool { return rec.has("unconfigured") })
	entry := rec.find("unconfigured")
	if entry.Stream != journal.StreamGateway || entry.Kind != "node" {
		t.Errorf("landed on %s/%s, want %s/node", entry.Stream, entry.Kind, journal.StreamGateway)
	}
	if entry.Level != journal.LevelWarn {
		t.Errorf("recorded at %v; a node that can serve nothing is worth a warning", entry.Level)
	}
	if entry.Keys.UserID != nodeOwner {
		t.Errorf("the entry names user %q, want %q", entry.Keys.UserID, nodeOwner)
	}
}

// Otherwise every laptop in the fleet gets a setup card every morning.
func TestAConfiguredNodeIsNotOfferedOnboarding(t *testing.T) {
	seen := &noticed{}
	rec := &recordingJournal{}
	dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}),
		claiming(claim([]string{"reviewer"}, "review-*")), journalling(rec), onboarding(nil, seen.record))

	waitFor(t, "the attach to be journalled", func() bool { return rec.has("attached") })
	if n := seen.count(); n != 0 {
		t.Fatalf("a configured node triggered onboarding %d times", n)
	}
	if rec.has("unconfigured") {
		t.Error("a configured node was journalled as unconfigured")
	}
}

// The gateway holds no profile bodies, so a bad reference can only be caught
// once a node says what it serves.
func TestConnectTimeValidationNamesTheProfilesTheFleetCannotServe(t *testing.T) {
	rec := &recordingJournal{}
	references := func() []config.AgentReference {
		return []config.AgentReference{
			{Field: "chat.defaults.agent", Name: "code"},
			{Field: "chat.channels[review-*].agent", Name: "reviewer"},
		}
	}
	dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}),
		claiming(claim([]string{"reviewer"}, "review-*")), journalling(rec), onboarding(references, nil))

	waitFor(t, "the unservable reference to be journalled", func() bool { return rec.has("unservable") })
	entry := rec.find("unservable")
	if entry.Level != journal.LevelWarn {
		t.Errorf("recorded at %v; this is advisory and must never be fatal", entry.Level)
	}
	agents, _ := entry.Payload["agents"].([]string)
	if len(agents) != 1 || agents[0] != "code" {
		t.Fatalf("reported %v as unservable, want exactly [code] — `reviewer` IS served and must not be named", agents)
	}
	served, _ := entry.Payload["served"].([]string)
	if len(served) != 1 || served[0] != "reviewer" {
		t.Errorf("the entry says the fleet serves %v, want [reviewer]", served)
	}
}

// A journal line on every healthy attach would be the same as no check.
func TestConnectTimeValidationIsSilentWhenTheFleetServesEverything(t *testing.T) {
	rec := &recordingJournal{}
	references := func() []config.AgentReference {
		return []config.AgentReference{{Field: "chat.defaults.agent", Name: "reviewer"}}
	}
	dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}),
		claiming(claim([]string{"reviewer"}, "review-*")), journalling(rec), onboarding(references, nil))

	waitFor(t, "the attach to be journalled", func() bool { return rec.has("attached") })
	if rec.has("unservable") {
		t.Error("a fully served configuration was reported as unservable")
	}
}

// The node's refusal keeps node admins in charge: a gateway can set up an
// empty node once, but never reconfigure a running one.
func TestConfigureReachesTheNodeAndTheNodeDecides(t *testing.T) {
	var (
		mu       sync.Mutex
		applied  []agentwire.NodeConfiguration
		occupied bool
	)
	apply := func(_ context.Context, cfg agentwire.NodeConfiguration) (agentwire.NodeConfigured, error) {
		mu.Lock()
		defer mu.Unlock()
		if occupied {
			return agentwire.NodeConfigured{}, errors.New("this node already has agent profiles and will not be reconfigured from a gateway")
		}
		applied = append(applied, cfg)
		occupied = true
		return agentwire.NodeConfigured{Applied: len(cfg.Agents), Restarting: true}, nil
	}
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}), configurable(apply))

	nodes := rig.host.Nodes()
	if len(nodes) != 1 {
		t.Fatalf("%d nodes attached, want 1", len(nodes))
	}
	node := nodes[0]
	body, err := json.Marshal(config.AgentProfile{Native: &config.NativeProfile{Provider: "anthropic", Model: "claude"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire := agentwire.NodeConfiguration{Agents: map[string]json.RawMessage{"code": body}}

	result, err := rig.host.Configure(context.Background(), node.NodeID, wire)
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if result.Applied != 1 || !result.Restarting {
		t.Errorf("node reported %+v, want 1 applied and a restart", result)
	}
	mu.Lock()
	got := len(applied)
	mu.Unlock()
	if got != 1 {
		t.Fatalf("the node applied %d configurations, want 1", got)
	}

	if _, err := rig.host.Configure(context.Background(), node.NodeID, wire); err == nil {
		t.Fatal("a configured node accepted a second configuration from its gateway")
	}
}

// The offer is a Slack-side entitlement that routes the owner's clicks; left
// open, an admin could never reach their own form again.
func TestAnOfferEndsWhenTheNodeIsConfiguredElsewhere(t *testing.T) {
	offered, settled := &noticed{}, &settledNodes{}
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}),
		onboarding(nil, offered.record), settling(settled.record))

	waitFor(t, "the unconfigured node to be noticed", func() bool { return offered.count() > 0 })
	if settled.count() != 0 {
		t.Fatal("a node that has nothing configured was reported as settled")
	}

	rig.claim.Publish(context.Background(), claim([]string{"reviewer"}, "review-*"))

	waitFor(t, "the offer to be withdrawn", func() bool { return settled.count() > 0 })
	if got := settled.first(); got.NodeID != offered.first().NodeID {
		t.Errorf("the withdrawal names node %q, want the node that was offered %q; "+
			"a mismatched id leaves the invitation standing", got.NodeID, offered.first().NodeID)
	}
	if got := settled.first(); got.UserID != nodeOwner {
		t.Errorf("the withdrawal names user %q, want the owner %q", got.UserID, nodeOwner)
	}
}

func TestAnOfferEndsWhenTheNodeGoesAway(t *testing.T) {
	offered, settled := &noticed{}, &settledNodes{}
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}),
		onboarding(nil, offered.record), settling(settled.record))

	waitFor(t, "the unconfigured node to be noticed", func() bool { return offered.count() > 0 })
	rig.host.DetachAll("the leader lock was taken by another node")

	waitFor(t, "the offer to be withdrawn", func() bool { return settled.count() > 0 })
	if got := settled.first(); got.NodeID != offered.first().NodeID {
		t.Errorf("the withdrawal names node %q, want the node that went away %q", got.NodeID, offered.first().NodeID)
	}
}

// The restart cancels the serving context, so firing it before the reply is
// written loses the answer; the hook blocks so that failure hangs the test.
func TestTheRestartWaitsForTheAnswerToBeOnTheWire(t *testing.T) {
	released := make(chan struct{})
	restarted := make(chan struct{})
	apply := func(_ context.Context, cfg agentwire.NodeConfiguration) (agentwire.NodeConfigured, error) {
		return agentwire.NodeConfigured{Applied: len(cfg.Agents), Restarting: true}, nil
	}
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}),
		configurable(apply),
		restarting(func() {
			close(restarted)
			<-released
		}))
	defer close(released)

	nodes := rig.host.Nodes()
	if len(nodes) != 1 {
		t.Fatalf("%d nodes attached, want 1", len(nodes))
	}
	body, err := json.Marshal(config.AgentProfile{Native: &config.NativeProfile{Provider: "anthropic", Model: "claude"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire := agentwire.NodeConfiguration{Agents: map[string]json.RawMessage{"code": body}}

	type answer struct {
		result agentwire.NodeConfigured
		err    error
	}
	answered := make(chan answer, 1)
	go func() {
		result, err := rig.host.Configure(context.Background(), nodes[0].NodeID, wire)
		answered <- answer{result, err}
	}()

	select {
	case got := <-answered:
		if got.err != nil {
			t.Fatalf("Configure: %v", got.err)
		}
		if !got.result.Restarting || got.result.Applied != 1 {
			t.Fatalf("the gateway was told %+v, want 1 applied and a restart", got.result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the node fired its restart before its answer was on the wire: " +
			"the restart cancels the context this connection is served on, so the operator " +
			"waiting in Slack sees a form that appeared to fail")
	}

	select {
	case <-restarted:
	case <-time.After(10 * time.Second):
		t.Fatal("a node that answered `restarting` never restarted; it would keep serving " +
			"the toolset it came up with, which is no toolset at all")
	}
}

type settledNodes struct {
	mu    sync.Mutex
	nodes []nodehost.Node
}

func (s *settledNodes) record(node nodehost.Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes = append(s.nodes, node)
}

func (s *settledNodes) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.nodes)
}

func (s *settledNodes) first() nodehost.Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.nodes) == 0 {
		return nodehost.Node{}
	}
	return s.nodes[0]
}

func TestConfiguringAnAbsentNodeSaysSo(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}))
	_, err := rig.host.Configure(context.Background(), "node-that-left", agentwire.NodeConfiguration{})
	if !errors.Is(err, agentruntime.ErrNoNode) {
		t.Fatalf("configuring an absent node gave %v, want ErrNoNode", err)
	}
}
