package nodehost_test

import (
	"context"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	configstore "github.com/miere/murtaugh/internal/config/store"
	"github.com/miere/murtaugh/internal/journal"
	"github.com/miere/murtaugh/internal/nodeclaim"
)

// This file is #195's verification, over the same loopback rig #193 and #194
// use: a real Host, real token verification, a real nodeserve.Server over
// 127.0.0.1, with only the agent and the credential store faked.
//
// There are two halves and the second is the one that matters. A round trip
// proves the transport. What #195 actually asks for is that a node-side
// CONFIGURATION CHANGE reaches the gateway without a reconnect — so the second
// test writes to the node's own configuration store and lets the node's own
// detector find it, and asserts that the connection never went down while that
// happened. Calling the advertiser directly would have proved the wire and left
// the crux of the item — that the node notices at all — untested.

func claim(profiles []string, matches ...string) agentwire.Advertisement {
	ad := agentwire.Advertisement{Profiles: profiles}
	for _, match := range matches {
		ad.Claims = append(ad.Claims, agentwire.AssignmentClaim{Match: match, Profile: profiles[0]})
	}
	return ad
}

// The connect-time claim rides the handshake answer, so the registry entry is
// complete at the instant it is published — never attached-and-mute.
func TestTheGatewayLearnsWhatANodeClaimsAtTheHandshake(t *testing.T) {
	want := claim([]string{"reviewer"}, "review-*", "C0999")
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}), claiming(want))

	nodes := rig.host.Nodes()
	if len(nodes) != 1 {
		t.Fatalf("the registry holds %d nodes, want 1", len(nodes))
	}
	if !reflect.DeepEqual(nodes[0].Advertisement, want) {
		t.Fatalf("the node is registered claiming %+v, want %+v", nodes[0].Advertisement, want)
	}
	if nodes[0].NodeID != "node-1" {
		t.Fatalf("the entry is keyed as %q; identity comes from the credential, not from anything the node said", nodes[0].NodeID)
	}
}

// A node that has never been configured claims nothing, and nothing is an
// answer rather than an absence — #170 makes it the trigger to onboard its
// owner.
func TestANodeThatClaimsNothingStillRegisters(t *testing.T) {
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}))
	nodes := rig.host.Nodes()
	if len(nodes) != 1 {
		t.Fatalf("the registry holds %d nodes, want 1", len(nodes))
	}
	if !nodes[0].Advertisement.Empty() {
		t.Fatalf("a silent node registered claiming %+v", nodes[0].Advertisement)
	}
}

// THE HARD HALF. A node-side configuration change reaches the gateway with no
// reconnect: the test writes a rule into the NODE's own configuration store,
// the node's own watcher notices, and the gateway's registry changes while the
// connection it changed over never went down.
//
// The two negative assertions are the point. Without them a reconnect would
// pass this test — the handshake carries the whole claim, so tearing the link
// down and dialling back in would produce exactly the same registry contents by
// exactly the wrong mechanism.
func TestAConfigurationChangeReachesTheGatewayWithoutAReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store, base := nodeConfigStore(t)
	putChat(t, store, config.ChatConfig{
		Enabled:  true,
		Defaults: config.ChatDefaults{Agent: "default"},
		Channels: config.ChannelRules{{Match: "nc-*", Agent: "default"}},
	})
	opening := loadClaim(t, store, base)

	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}), claiming(opening))
	waitFor(t, "the opening claim to register", func() bool {
		nodes := rig.host.Nodes()
		return len(nodes) == 1 && len(nodes[0].Advertisement.Claims) == 1
	})
	attachedAt := rig.host.Nodes()[0].AttachedAt

	// The node's own detector, over its own store, on its own goroutine. The
	// interval is injected rather than the poll being reached past, so what is
	// under test is the thing that will run in production.
	watcher, err := nodeclaim.NewWatcher(ctx, nodeclaim.Options{
		Store:    store,
		Base:     base,
		Serving:  []string{"default"},
		Publish:  rig.claim.Publish,
		Interval: 10 * time.Millisecond,
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("watcher: %v", err)
	}
	go watcher.Run(ctx)

	// The node admin edits their own node. No approval card, no rollback: on a
	// node they are the authority.
	putChat(t, store, config.ChatConfig{
		Enabled:  true,
		Defaults: config.ChatDefaults{Agent: "default"},
		Channels: config.ChannelRules{
			{Match: "nc-*", Agent: "default"},
			{Match: "review-*", Agent: "default"},
		},
	})

	waitFor(t, "the gateway to learn the new claim", func() bool {
		nodes := rig.host.Nodes()
		return len(nodes) == 1 && len(nodes[0].Advertisement.Claims) == 2
	})

	node := rig.host.Nodes()[0]
	if node.Advertisement.Claims[1].Match != "review-*" {
		t.Fatalf("the gateway learned %+v; the node's own rule order is the only precedence there is", node.Advertisement.Claims)
	}
	// Nothing reconnected. A fresh connection would have a later attach time
	// and a different registry entry behind it.
	if !node.AttachedAt.Equal(attachedAt) {
		t.Fatal("the registry entry was replaced; the change was carried by a reconnect, which is the thing this must not do")
	}
	select {
	case <-rig.nodeStopped:
		t.Fatal("the node's connection ended; a configuration change must not cost a reconnect")
	default:
	}
}

// A claim routing to a profile this node does not serve never leaves the node.
// The gateway acting on it would delegate a conversation to a machine that
// answers with an error, and the node is the only side that knows.
func TestAClaimForAProfileTheNodeDoesNotServeIsNotSent(t *testing.T) {
	store, base := nodeConfigStore(t)
	putChat(t, store, config.ChatConfig{
		Enabled:  true,
		Defaults: config.ChatDefaults{Agent: "default"},
		Channels: config.ChannelRules{
			{Match: "nc-*", Agent: "default"},
			{Match: "ops-*", Agent: "ops"},
		},
	})

	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}), claiming(loadClaim(t, store, base)))

	nodes := rig.host.Nodes()
	if len(nodes[0].Advertisement.Claims) != 1 || nodes[0].Advertisement.Claims[0].Match != "nc-*" {
		t.Fatalf("the gateway was told %+v; a node must not claim a channel it cannot take", nodes[0].Advertisement.Claims)
	}
}

// #170: "Disconnects are journalled, not announced." A laptop sleeping at six
// o'clock disconnects every evening, and a nightly DM teaches the one admin who
// would act on the message that matters to ignore it. So both ends of a node's
// life land on the gateway stream, where a query finds them, and nowhere a user
// is interrupted by them.
func TestANodesArrivalAndDepartureAreJournalled(t *testing.T) {
	rec := &recordingJournal{}
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}),
		claiming(claim([]string{"reviewer"}, "review-*")), journalling(rec))

	waitFor(t, "the attach to be journalled", func() bool { return rec.has("attached") })
	attached := rec.find("attached")
	if attached.Stream != journal.StreamGateway || attached.Kind != "node" {
		t.Fatalf("the attach landed on %s/%s, want %s/node", attached.Stream, attached.Kind, journal.StreamGateway)
	}
	if attached.Keys.UserID == "" {
		t.Fatal("the attach names no user; a fleet is a user's, and an unattributed entry cannot be queried as one")
	}
	if got := attached.Payload["claims"]; got == nil {
		t.Fatal("the attach records no claims; what a node said it would take is the half worth reading back")
	}

	if err := rig.host.CloseCredential(context.Background(), rig.selector); err != nil {
		t.Fatalf("close credential: %v", err)
	}
	waitFor(t, "the detach to be journalled", func() bool { return rec.has("detached") })

	// A revocation is its own record, and it is the one an auditor comes for:
	// "detached" is what a laptop lid does every evening, and a connection ended
	// because somebody revoked its credential must be distinguishable from that
	// without inferring it from a timestamp.
	revoked := rec.find("revoked")
	if revoked.Kind == "" {
		t.Fatal("a credential revocation closed a live connection and left no journal record; nothing about a node is announced, so this is the only trace it leaves")
	}
	if revoked.Stream != journal.StreamGateway || revoked.Kind != "node" {
		t.Fatalf("the revocation landed on %s/%s, want %s/node", revoked.Stream, revoked.Kind, journal.StreamGateway)
	}
	if revoked.Level != journal.LevelWarn {
		t.Fatalf("the revocation was recorded at %q; it is the one node event that is not routine", revoked.Level)
	}
	if revoked.Payload["selector"] != rig.selector {
		t.Fatalf("the revocation names selector %v, want %q — the credential is what was revoked", revoked.Payload["selector"], rig.selector)
	}
}

// A node changing what it claims is journalled too, and the opening claim is
// NOT: it is part of the arrival, and a second line saying the same thing is
// the noise that makes a journal unreadable.
//
// The claim is what delegation will act on, so a change to it is a change to
// where conversations go — and #170 says these are journalled rather than
// announced, which makes this record the only trace an operator has when a
// channel starts landing somewhere new.
func TestAChangedClaimIsJournalledAndTheOpeningOneIsNot(t *testing.T) {
	rec := &recordingJournal{}
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}),
		claiming(claim([]string{"reviewer"}, "review-*")), journalling(rec))
	waitFor(t, "the attach to be journalled", func() bool { return rec.has("attached") })

	if n := rec.count("advertised"); n != 0 {
		t.Fatalf("the opening claim produced %d re-advertisement records; it is already part of the arrival", n)
	}

	// The node's own advertiser, which is what nodeclaim.Watcher drives when a
	// node admin edits their node's configuration.
	rig.claim.Publish(context.Background(), claim([]string{"reviewer"}, "review-*", "nc-*"))

	waitFor(t, "the changed claim to be journalled", func() bool { return rec.has("advertised") })
	advertised := rec.find("advertised")
	if advertised.Stream != journal.StreamGateway || advertised.Kind != "node" {
		t.Fatalf("the re-advertisement landed on %s/%s, want %s/node", advertised.Stream, advertised.Kind, journal.StreamGateway)
	}
	claims, _ := advertised.Payload["claims"].([]string)
	if len(claims) != 2 || claims[1] != "nc-*" {
		t.Fatalf("the record carries claims %v; what the node now claims is the whole content of the event", advertised.Payload["claims"])
	}
	if n := rec.count("advertised"); n != 1 {
		t.Fatalf("one configuration change produced %d records", n)
	}
}

// recordingJournal keeps what the Host recorded, decoded far enough to assert
// on. It is a fake because internal/journal's own store has its own tests; what
// is under test here is what nodehost hands it.
type recordingJournal struct {
	mu      sync.Mutex
	entries []recordedEvent
}

type recordedEvent struct {
	Stream  string
	Kind    string
	Level   journal.Level
	Keys    journal.Keys
	Payload map[string]any
}

func (r *recordingJournal) Record(_ context.Context, ev journal.Event) {
	payload, _ := ev.Payload.(map[string]any)
	r.mu.Lock()
	r.entries = append(r.entries, recordedEvent{Stream: ev.Stream, Kind: ev.Kind, Level: ev.Level, Keys: ev.Keys, Payload: payload})
	r.mu.Unlock()
}

func (r *recordingJournal) has(state string) bool {
	return r.find(state).Kind != ""
}

func (r *recordingJournal) count(state string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, entry := range r.entries {
		if entry.Payload["state"] == state {
			n++
		}
	}
	return n
}

func (r *recordingJournal) find(state string) recordedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, entry := range r.entries {
		if entry.Payload["state"] == state {
			return entry
		}
	}
	return recordedEvent{}
}

// nodeConfigStore opens a real SQLite configuration store for a node, with one
// agent profile in it. It is the node's own store: nothing in this file touches
// the gateway's.
func nodeConfigStore(t *testing.T) (config.Store, config.Config) {
	t.Helper()
	dir := t.TempDir()
	base := config.Config{
		BaseDir:  dir,
		BaseName: "config",
		// A node's configuration is validated as a whole, which today still
		// includes the Slack credentials it will never use. That is the
		// configuration split (#170 item 12), not this item.
		OAuth: config.OAuthConfig{AppToken: "xapp-test", BotToken: "xoxb-test"},
		Database: config.DatabaseConfig{
			Backend: "sqlite",
			SQLite:  config.SQLiteConfig{Path: filepath.Join(dir, "config.db")},
		},
	}
	store, err := configstore.Open(context.Background(), base.Database, dir, base.BaseName)
	if err != nil {
		t.Fatalf("open the node's config store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// Two profiles, one of which this node's process serves. That is the
	// ordinary shape — cmd/murtaugh-runtime picks one agent, because the
	// protocol carries no agent name — and it is what makes "advertise what you
	// serve, not what you have configured" a distinction with teeth.
	for _, name := range []string{"default", "ops"} {
		profile := config.AgentProfile{WorkDir: dir, Native: &config.NativeProfile{Provider: "anthropic", Model: "claude-sonnet-4", APIKeyEnv: "ANTHROPIC_API_KEY"}}
		if err := store.UpsertItem(context.Background(), config.SectionAgent, name, profile); err != nil {
			t.Fatalf("write an agent profile: %v", err)
		}
	}
	return store, base
}

func putChat(t *testing.T, store config.Store, chat config.ChatConfig) {
	t.Helper()
	if err := store.PutSingleton(context.Background(), config.SingletonChat, chat); err != nil {
		t.Fatalf("write chat config: %v", err)
	}
}

// loadClaim is what cmd/murtaugh-runtime does before its first dial: assemble
// the configuration and derive the claim from it.
func loadClaim(t *testing.T, store config.Store, base config.Config) agentwire.Advertisement {
	t.Helper()
	cfg, err := store.Load(context.Background(), base)
	if err != nil {
		t.Fatalf("load the node's config: %v", err)
	}
	return nodeclaim.Advertise(cfg, []string{"default"})
}

// A reconnect re-advertises from scratch. That is what makes dropping a push
// while nothing is attached the right behaviour rather than a compromise: a
// queued claim would be replayed at the exact moment a current one is already
// riding the new handshake.
func TestAReconnectReAdvertisesFromScratch(t *testing.T) {
	want := claim([]string{"reviewer"}, "review-*")
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}), claiming(want))

	redial(t, rig)

	waitFor(t, "the new connection to carry the claim", func() bool {
		nodes := rig.host.Nodes()
		return len(nodes) == 1 && reflect.DeepEqual(nodes[0].Advertisement, want)
	})
}
