package nodehost

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	configstore "github.com/miere/murtaugh/internal/config/store"
)

// This file is #196's verification. It is an in-package test because what is
// under test is the registry's own arithmetic — which of the connected nodes
// takes a conversation — and the registry entry is a socket plus a claim. The
// socket is the part that is not being tested here, so the entries below are
// real *attached values with no client on them; nothing on the election path
// touches one, which is itself a property worth having.
//
// The loopback rig next door covers the other half: a real connection, a real
// handshake, a real turn.

// nodeStub describes a connected node for a table row.
type nodeStub struct {
	id     string
	owner  string
	claims []string
}

// attachStubs publishes stub connections into a Host's registry.
//
// It goes through insert, so the entries are keyed and displaced exactly as a
// real handshake's would be — a test that wrote h.nodes directly would not
// notice if publication ever stopped being what makes a node electable.
func attachStubs(h *Host, stubs ...nodeStub) map[string]*attached {
	out := make(map[string]*attached, len(stubs))
	for i, stub := range stubs {
		claims := make([]agentwire.AssignmentClaim, 0, len(stub.claims))
		for _, match := range stub.claims {
			claims = append(claims, agentwire.AssignmentClaim{Match: match, Profile: "default"})
		}
		node := &attached{
			connID:     stub.id + "-conn",
			selector:   stub.id + "-sel",
			nodeID:     stub.id,
			userID:     stub.owner,
			attachedAt: time.Unix(int64(1000+i), 0),
			closed:     make(chan struct{}),
			ad:         agentwire.Advertisement{Profiles: []string{"default"}, Claims: claims},
		}
		h.insert(node)
		out[stub.id] = node
	}
	return out
}

// newTestHost builds a Host with no listener and no credentials to verify. The
// credential store is registry_internal_test.go's, because nothing in this file
// authenticates: nothing in this file dials.
func newTestHost(t *testing.T, pins config.ConversationPinStore, access config.AccessConfig) *Host {
	t.Helper()
	host, err := New(Options{
		Tokens: noTokens{},
		Logger: quietLogger(),
		Pins:   pins,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	host.setAccess(access)
	return host
}

// TestTheWorkedExampleFromTheSpec is #170's table, row for row.
//
// Node A claims channels beginning `nc-`; node B claims those beginning
// `review-`. The pin store is deliberately absent, so each call is a fresh
// election and the round robin is observable — with a pin in place the second
// call would answer from the pin and prove nothing about step 3 or step 4.
func TestTheWorkedExampleFromTheSpec(t *testing.T) {
	const owner = "U1"
	for _, tc := range []struct {
		name        string
		channelID   string
		channelName string
		// want is successive elections. One entry asserts "that node takes it";
		// two assert the rotation, which is the only way to tell a round robin
		// from a node that simply always wins.
		want []string
	}{
		{
			name: "nc-releases matches A only, so A takes it",
			// The channel id is not claimed by anybody: the match is on the
			// NAME, which is what the whole worked example is written in.
			channelID: "C100", channelName: "nc-releases",
			want: []string{"node-a", "node-a"},
		},
		{
			name:      "review-pr-812 matches B only, so B takes it",
			channelID: "C101", channelName: "review-pr-812",
			want: []string{"node-b", "node-b"},
		},
		{
			name:      "support-discussions matches neither, so it round robins over the whole fleet",
			channelID: "C102", channelName: "support-discussions",
			want: []string{"node-a", "node-b", "node-a"},
		},
		{
			name:      "a channel both claim round robins over the matching nodes",
			channelID: "C103", channelName: "shared-desk",
			want: []string{"node-a", "node-b", "node-a"},
		},
		{
			name: "a DM claims nothing, so it round robins too",
			// #170's algorithm already round-robins an unclaimed conversation,
			// which is the intended DM behaviour: every node's chat defaults
			// answer every DM, so a DM claim would select nothing.
			channelID: "D200", channelName: "",
			want: []string{"node-a", "node-b"},
		},
		{
			name: "an unresolved channel name can still match an exact id claim",
			// The cold-cache case. Only an exact channel-id claim can match, and
			// node B carries one.
			channelID: "C104", channelName: "",
			want: []string{"node-b", "node-b"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := newTestHost(t, nil, config.AccessConfig{})
			attachStubs(host,
				nodeStub{id: "node-a", owner: owner, claims: []string{"nc-*", "shared-desk"}},
				nodeStub{id: "node-b", owner: owner, claims: []string{"review-*", "shared-desk", "C104"}},
			)
			ctx := agent.WithConversation(context.Background(),
				agent.ConversationKey{ChannelID: tc.channelID, ThreadTS: "1.1"})
			meta := agent.SessionMetadata{
				ChannelID: tc.channelID, ChannelName: tc.channelName, ThreadTS: "1.1", UserID: owner,
			}
			for i, want := range tc.want {
				got, err := host.delegate(ctx, meta)
				if err != nil {
					t.Fatalf("election %d: %v", i+1, err)
				}
				if got.node.nodeID != want {
					t.Fatalf("election %d went to %q, want %q", i+1, got.node.nodeID, want)
				}
				if got.takeover {
					t.Fatalf("election %d reported a takeover on a conversation that was never pinned", i+1)
				}
			}
		})
	}
}

// Step 3 round robins among the nodes that CLAIMED the channel, and the fleet's
// other nodes take none of it.
//
// #170's worked example cannot express this: its fleet is two nodes and both
// claim the shared channel, so `matching` and `fleet` are the same set in every
// row that reaches this branch and step 3 is indistinguishable from step 4. A
// third node that claims nothing is what separates them — and getting it wrong
// sends a third of a claimed channel's traffic to a machine that never asked
// for it, which reads as a node-side assignment bug rather than as a gateway
// one.
func TestAClaimedChannelRoundRobinsOnlyAmongTheNodesThatClaimedIt(t *testing.T) {
	const owner = "U1"
	host := newTestHost(t, nil, config.AccessConfig{})
	attachStubs(host,
		nodeStub{id: "node-a", owner: owner, claims: []string{"nc-*"}},
		nodeStub{id: "node-b", owner: owner, claims: []string{"nc-*"}},
		// In the fleet, connected, and claiming something else entirely.
		nodeStub{id: "node-c", owner: owner, claims: []string{"other-*"}},
	)

	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		seen[elected(t, host, "C100", "nc-releases", owner)]++
	}
	if seen["node-c"] != 0 {
		t.Fatalf("a node that claims nothing on this channel took %d of 6 elections: %v", seen["node-c"], seen)
	}
	if seen["node-a"] != 3 || seen["node-b"] != 3 {
		t.Fatalf("the two nodes that claimed it did not share it evenly: %v", seen)
	}
}

// TestTheFleetIsOwnNodesOrGrantedNodesButNeverAMixture pins down the rule that
// makes user choice and node claims incapable of conflicting.
func TestTheFleetIsOwnNodesOrGrantedNodesButNeverAMixture(t *testing.T) {
	access := config.AccessConfig{NodeGrants: map[string][]string{
		// U2 lets U1 and U3 run on their node.
		"node-b": {"U1", "U3"},
	}}

	t.Run("a user with a node of their own never touches a granted one", func(t *testing.T) {
		host := newTestHost(t, nil, access)
		attachStubs(host,
			nodeStub{id: "node-a", owner: "U1"},
			nodeStub{id: "node-b", owner: "U2"},
		)
		// Nobody claims this channel, so an unbounded fleet would round robin
		// across both and land on node-b on the second call. It must not: U1
		// owns node-a, so node-a IS the fleet.
		for i := 0; i < 4; i++ {
			got := elected(t, host, "C1", "anything", "U1")
			if got != "node-a" {
				t.Fatalf("election %d escaped the user's own fleet: %q", i+1, got)
			}
		}
	})

	t.Run("a user with no node of their own falls back to what they were granted", func(t *testing.T) {
		host := newTestHost(t, nil, access)
		attachStubs(host,
			nodeStub{id: "node-a", owner: "U2"},
			nodeStub{id: "node-b", owner: "U2"},
		)
		// node-a is U2's and carries no grant; node-b carries one for U3.
		for i := 0; i < 4; i++ {
			got := elected(t, host, "C1", "anything", "U3")
			if got != "node-b" {
				t.Fatalf("election %d used a node U3 holds no grant on: %q", i+1, got)
			}
		}
	})

	t.Run("a user with neither is told so, and not handed somebody else's machine", func(t *testing.T) {
		host := newTestHost(t, nil, access)
		attachStubs(host, nodeStub{id: "node-a", owner: "U2"})
		ctx := agent.WithConversation(context.Background(), agent.ConversationKey{ChannelID: "C1"})
		_, err := host.delegate(ctx, agent.SessionMetadata{ChannelID: "C1", UserID: "U9"})
		if !errors.Is(err, ErrNoFleet) {
			t.Fatalf("want ErrNoFleet, got %v", err)
		}
		// Not ErrNoNode: something IS connected, and telling this user that
		// nothing is would send them to the wrong person.
		if errors.Is(err, ErrNoNode) {
			t.Fatal("a user with no fleet was told the gateway has no nodes at all")
		}
	})

	t.Run("nothing connected is a different answer from nothing of yours", func(t *testing.T) {
		host := newTestHost(t, nil, access)
		ctx := agent.WithConversation(context.Background(), agent.ConversationKey{ChannelID: "C1"})
		_, err := host.delegate(ctx, agent.SessionMetadata{ChannelID: "C1", UserID: "U1"})
		if !errors.Is(err, ErrNoNode) {
			t.Fatalf("want ErrNoNode, got %v", err)
		}
	})
}

// TestAPinSurvivesAGatewayFailover is the property that makes turn two land
// where turn one did even when the gateway serving turn one is gone.
//
// It uses the real SQLite pin store, opened twice: once by the gateway that
// elects and once by the gateway that takes over. An in-memory fake would pass
// this test without the pin ever reaching a disk.
func TestAPinSurvivesAGatewayFailover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.db")
	openPins := func() config.ConversationPinStore {
		pins, err := configstore.OpenConversationPins(context.Background(),
			config.DatabaseConfig{Backend: config.BackendSQLite, SQLite: config.SQLiteConfig{Path: path}}, "", "")
		if err != nil {
			t.Fatalf("OpenConversationPins: %v", err)
		}
		t.Cleanup(func() { _ = pins.Close() })
		return pins
	}
	fleet := []nodeStub{
		{id: "node-a", owner: "U1"},
		{id: "node-b", owner: "U1"},
	}
	ctx := agent.WithConversation(context.Background(),
		agent.ConversationKey{TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1"})
	meta := agent.SessionMetadata{TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", UserID: "U1"}

	// The gateway that elects. Nobody claims C1, so this is a round robin and
	// the outcome is genuinely a choice rather than the only option.
	first := newTestHost(t, openPins(), config.AccessConfig{})
	attachStubs(first, fleet...)
	initial, err := first.delegate(ctx, meta)
	if err != nil {
		t.Fatalf("first election: %v", err)
	}

	// The gateway that was promoted after it died. Fresh Host, fresh cursor,
	// fresh registry — everything except the store.
	second := newTestHost(t, openPins(), config.AccessConfig{})
	attachStubs(second, fleet...)
	for i := 0; i < 3; i++ {
		again, err := second.delegate(ctx, meta)
		if err != nil {
			t.Fatalf("turn %d after failover: %v", i+1, err)
		}
		if again.node.nodeID != initial.node.nodeID {
			t.Fatalf("the conversation moved across a failover: %q then %q",
				initial.node.nodeID, again.node.nodeID)
		}
		if again.takeover {
			t.Fatal("a conversation whose node is still connected was reported as a takeover")
		}
	}
}

// TestReElectionOverwritesTheStoredPin is the failure #170 names: a pin left
// pointing at a machine that is gone re-elects on every turn, and every turn
// lands somewhere new.
func TestReElectionOverwritesTheStoredPin(t *testing.T) {
	pins := &memPins{}
	host := newTestHost(t, pins, config.AccessConfig{})
	nodes := attachStubs(host,
		nodeStub{id: "node-a", owner: "U1"},
		nodeStub{id: "node-b", owner: "U1"},
	)
	ref := config.ConversationRef{TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1"}
	ctx := agent.WithConversation(context.Background(),
		agent.ConversationKey{TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1"})
	meta := agent.SessionMetadata{TeamID: "T1", ChannelID: "C1", ThreadTS: "1.1", UserID: "U1"}

	first, err := host.delegate(ctx, meta)
	if err != nil {
		t.Fatalf("first election: %v", err)
	}
	gone := first.node.nodeID

	// The elected node disconnects.
	host.remove(nodes[gone])

	second, err := host.delegate(ctx, meta)
	if err != nil {
		t.Fatalf("re-election: %v", err)
	}
	if second.node.nodeID == gone {
		t.Fatalf("the conversation was re-elected onto the node that left: %q", gone)
	}
	if !second.takeover || second.previous != gone {
		t.Fatalf("the move was not reported as a takeover from %q: %+v", gone, second)
	}

	// The assertion that matters is on the STORE, not on the routing. Routing
	// correctly while leaving the dead node's row behind is exactly the bug:
	// the next turn would re-elect again, and the one after that, for ever.
	stored, found, err := pins.Get(context.Background(), ref)
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if stored.NodeID != second.node.nodeID {
		t.Fatalf("the stored pin still names %q; it should name %q", stored.NodeID, second.node.nodeID)
	}

	// And the conversation now STAYS there, which is the other half of "does
	// not flap": a third turn must be answered from the pin, not re-elected.
	third, err := host.delegate(ctx, meta)
	if err != nil {
		t.Fatalf("third turn: %v", err)
	}
	if third.node.nodeID != second.node.nodeID {
		t.Fatalf("the conversation moved again: %q then %q", second.node.nodeID, third.node.nodeID)
	}
	if third.takeover {
		t.Fatal("the takeover was announced twice")
	}
	if pins.puts != 2 {
		t.Fatalf("the pin was written %d times; want 2 — once elected, once re-elected", pins.puts)
	}
}

// TestAPinnedConversationIsNotReFleeted covers the collision the conversation
// key creates on purpose: it omits the user, so the channel's second speaker
// rides the first speaker's node rather than dragging the conversation onto
// their own.
func TestAPinnedConversationIsNotReFleeted(t *testing.T) {
	host := newTestHost(t, &memPins{}, config.AccessConfig{})
	attachStubs(host,
		nodeStub{id: "node-a", owner: "U1"},
		nodeStub{id: "node-b", owner: "U2"},
	)
	ctx := agent.WithConversation(context.Background(),
		agent.ConversationKey{ChannelID: "C1", ThreadTS: "1.1"})

	first, err := host.delegate(ctx, agent.SessionMetadata{ChannelID: "C1", ThreadTS: "1.1", UserID: "U1"})
	if err != nil {
		t.Fatalf("U1's election: %v", err)
	}
	if first.node.nodeID != "node-a" {
		t.Fatalf("U1's conversation went to %q, not their own node", first.node.nodeID)
	}
	// U2 speaks next in the same channel thread. Their own node is connected,
	// and the conversation still does not move: a fleet decides an election, a
	// pin decides a turn.
	second, err := host.delegate(ctx, agent.SessionMetadata{ChannelID: "C1", ThreadTS: "1.1", UserID: "U2"})
	if err != nil {
		t.Fatalf("U2's turn: %v", err)
	}
	if second.node.nodeID != "node-a" {
		t.Fatalf("the second speaker moved the conversation to %q", second.node.nodeID)
	}
}

// TestAConversationWithNoKeyIsNotPinned covers the callers #170 defers to item
// 13: a job, an unfurl or a workflow trigger has no conversation, and must not
// have one invented for it.
func TestAConversationWithNoKeyIsNotPinned(t *testing.T) {
	pins := &memPins{}
	host := newTestHost(t, pins, config.AccessConfig{})
	attachStubs(host, nodeStub{id: "node-a", owner: "U1"})

	if _, err := host.delegate(context.Background(),
		agent.SessionMetadata{UserID: "U1", Ephemeral: true, Source: "job"}); err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if pins.puts != 0 {
		t.Fatalf("a keyless delegation wrote %d pins", pins.puts)
	}
}

// TestAnUnreadablePinFailsTheTurnRatherThanMovingIt states the choice made when
// the pin store is broken: a database hiccup must not look to the user like the
// agent forgetting the conversation.
func TestAnUnreadablePinFailsTheTurnRatherThanMovingIt(t *testing.T) {
	boom := errors.New("the pin store is down")
	host := newTestHost(t, &memPins{getErr: boom}, config.AccessConfig{})
	attachStubs(host, nodeStub{id: "node-a", owner: "U1"}, nodeStub{id: "node-b", owner: "U1"})
	ctx := agent.WithConversation(context.Background(), agent.ConversationKey{ChannelID: "C1"})

	if _, err := host.delegate(ctx, agent.SessionMetadata{ChannelID: "C1", UserID: "U1"}); !errors.Is(err, boom) {
		t.Fatalf("want the store's error, got %v", err)
	}
}

// TestAnUnwritablePinStillServesTheTurn is the other side of that choice: the
// node is chosen and the conversation is servable, so denying it over
// bookkeeping would be the wrong trade.
func TestAnUnwritablePinStillServesTheTurn(t *testing.T) {
	host := newTestHost(t, &memPins{putErr: errors.New("disk full")}, config.AccessConfig{})
	attachStubs(host, nodeStub{id: "node-a", owner: "U1"})
	ctx := agent.WithConversation(context.Background(), agent.ConversationKey{ChannelID: "C1"})

	got, err := host.delegate(ctx, agent.SessionMetadata{ChannelID: "C1", UserID: "U1"})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if got.node.nodeID != "node-a" {
		t.Fatalf("elected %q", got.node.nodeID)
	}
}

// TestARotatingNodeIsNotRoundRobinnedAgainstItself covers the registry's
// connection-vs-node split from delegation's side: during a credential rotation
// one machine holds two live connections, and a fleet that listed it twice
// would give it two thirds of a two-node rotation and call that balance.
func TestARotatingNodeIsNotRoundRobinnedAgainstItself(t *testing.T) {
	host := newTestHost(t, nil, config.AccessConfig{})
	attachStubs(host,
		nodeStub{id: "node-a", owner: "U1"},
		nodeStub{id: "node-b", owner: "U1"},
	)
	// node-a dials back in on its NEW credential while the old connection is
	// still up. Different selector, so both connections stay.
	host.insert(&attached{
		connID: "node-a-conn-2", selector: "node-a-sel-2", nodeID: "node-a", userID: "U1",
		attachedAt: time.Unix(2000, 0), closed: make(chan struct{}),
	})

	if got := len(host.connected()); got != 2 {
		t.Fatalf("the fleet has %d entries; one machine is in it twice", got)
	}
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		seen[elected(t, host, "C1", "unclaimed", "U1")]++
	}
	if seen["node-a"] != 2 || seen["node-b"] != 2 {
		t.Fatalf("the rotation was unbalanced by the extra connection: %v", seen)
	}
}

// elected runs one election and returns the node id.
func elected(t *testing.T, host *Host, channelID, channelName, userID string) string {
	t.Helper()
	ctx := agent.WithConversation(context.Background(), agent.ConversationKey{ChannelID: channelID})
	got, err := host.delegate(ctx, agent.SessionMetadata{
		ChannelID: channelID, ChannelName: channelName, UserID: userID,
	})
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	return got.node.nodeID
}

// memPins is the pin store as a map, with the failure injection the two error
// tests need. The real store's behaviour is covered against all three backends
// in internal/config/store.
type memPins struct {
	mu     sync.Mutex
	rows   map[config.ConversationRef]config.ConversationPin
	puts   int
	getErr error
	putErr error
}

func (p *memPins) Get(_ context.Context, ref config.ConversationRef) (config.ConversationPin, bool, error) {
	if p.getErr != nil {
		return config.ConversationPin{}, false, p.getErr
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pin, ok := p.rows[ref]
	return pin, ok, nil
}

func (p *memPins) Put(_ context.Context, pin config.ConversationPin) error {
	if p.putErr != nil {
		return p.putErr
	}
	if err := pin.Validate(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rows == nil {
		p.rows = map[config.ConversationRef]config.ConversationPin{}
	}
	p.rows[pin.Conversation] = pin
	p.puts++
	return nil
}

func (p *memPins) Close() error { return nil }

// TestTheTakeoverNoticeLandsInsideTheUserMessage is the placement assertion
// #196 asks for.
//
// The invariant it protects is named assertNoConsecutiveUserAfterTool: a
// standalone user message appended after a tool-result is the consecutive-user
// empty-reply bug, and native.Conversation exposes no way to append per-turn
// context as its own message precisely so nothing can do it by accident. The
// notice therefore has to be part of the prompt text.
func TestTheTakeoverNoticeLandsInsideTheUserMessage(t *testing.T) {
	req := agent.PromptRequest{
		Text:    "what did we decide about the retry budget?",
		History: "<thread>\nalice: earlier message\n</thread>",
		Channel: "C1", Thread: "1.1", User: "U1",
	}
	got := foldTakeover(req, "node-a")

	if !strings.HasPrefix(got.Text, "<"+takeoverTag+">") {
		t.Fatalf("the notice is not at the head of the user message:\n%s", got.Text)
	}
	if !strings.Contains(got.Text, "</"+takeoverTag+">") {
		t.Fatal("the notice is not closed")
	}
	if !strings.HasSuffix(got.Text, req.Text) {
		t.Fatalf("the user's own words are not the tail of the message:\n%s", got.Text)
	}
	// Not in History: a backend is free to emit History as its own content
	// block, which would put the notice in exactly the standalone position the
	// invariant forbids.
	if got.History != req.History {
		t.Fatal("the notice was folded into the thread transcript instead of the user message")
	}
	// Not <context>: native and ACP already emit a block by that name in the
	// same message, and two would read as a malformed one.
	if strings.Contains(got.Text, "<context>") {
		t.Fatal("the notice reused the tag the backends already emit")
	}
	if !strings.Contains(got.Text, "node-a") {
		t.Fatal("the notice does not say where the conversation came from")
	}

	// A caption-less upload is a real prompt with no text. The notice must
	// still be the message rather than produce a leading blank line.
	only := foldTakeover(agent.PromptRequest{}, "")
	if strings.TrimSpace(only.Text) != only.Text || !strings.HasPrefix(only.Text, "<"+takeoverTag+">") {
		t.Fatalf("an empty prompt produced a malformed message: %q", only.Text)
	}
}

// TestTheTakeoverNoticeIsSentOnceAndOnlyOnce guards the other half of the
// placement: a model told on every message that it has just arrived and can see
// nothing behaves as though that were true.
func TestTheTakeoverNoticeIsSentOnceAndOnlyOnce(t *testing.T) {
	host := newTestHost(t, nil, config.AccessConfig{})
	host.markTakeover("session-1", "node-a")

	first := host.preparePrompt("session-1", agent.PromptRequest{Text: "hello"})
	if !strings.Contains(first.Text, takeoverTag) {
		t.Fatal("the first prompt after a takeover did not carry the notice")
	}
	second := host.preparePrompt("session-1", agent.PromptRequest{Text: "hello"})
	if strings.Contains(second.Text, takeoverTag) {
		t.Fatal("the notice was repeated on a later turn")
	}
	// A session that never moved carries nothing.
	other := host.preparePrompt("session-2", agent.PromptRequest{Text: "hello"})
	if other.Text != "hello" {
		t.Fatalf("an untouched prompt was modified: %q", other.Text)
	}
}

// TestASessionIsBoundToTheNodeThatMintedIt covers the binding that keeps a warm
// turn on its node, and the error that starts the recovery when that node has
// gone.
func TestASessionIsBoundToTheNodeThatMintedIt(t *testing.T) {
	host := newTestHost(t, nil, config.AccessConfig{})
	nodes := attachStubs(host, nodeStub{id: "node-a", owner: "U1"})
	host.bindSession("s1", nodes["node-a"])

	if got, err := host.sessionNode("s1"); err != nil || got != nodes["node-a"] {
		t.Fatalf("sessionNode: %v %v", got, err)
	}
	if _, err := host.sessionNode("unknown"); !errors.Is(err, agent.ErrSessionGone) {
		t.Fatalf("an unknown session gave %v, want ErrSessionGone", err)
	}

	host.remove(nodes["node-a"])
	// ErrSessionGone, not ErrNoNode: this session cannot run and a new one can,
	// which is what the session manager needs in order to re-elect.
	_, err := host.sessionNode("s1")
	if !errors.Is(err, agent.ErrSessionGone) {
		t.Fatalf("a session whose node left gave %v, want ErrSessionGone", err)
	}
	if errors.Is(err, ErrNoNode) {
		t.Fatal("a lost session was reported as a gateway with no nodes")
	}
}
