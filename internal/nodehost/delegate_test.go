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

type nodeStub struct {
	id     string
	owner  string
	claims []string
}

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

// No pin store on purpose: every call is then a fresh election, so the round
// robin can be observed.
func TestTheWorkedExampleFromTheSpec(t *testing.T) {
	const owner = "U1"
	for _, tc := range []struct {
		name        string
		channelID   string
		channelName string
		want        []string
	}{
		{
			name:      "nc-releases matches A only, so A takes it",
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
			name:      "a DM claims nothing, so it round robins too",
			channelID: "D200", channelName: "",
			want: []string{"node-a", "node-b"},
		},
		{
			name:      "an unresolved channel name can still match an exact id claim",
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

// The third node, which claims nothing, is what makes this test mean anything:
// with only claiming nodes, the claimers and the whole fleet are the same set.
func TestAClaimedChannelRoundRobinsOnlyAmongTheNodesThatClaimedIt(t *testing.T) {
	const owner = "U1"
	host := newTestHost(t, nil, config.AccessConfig{})
	attachStubs(host,
		nodeStub{id: "node-a", owner: owner, claims: []string{"nc-*"}},
		nodeStub{id: "node-b", owner: owner, claims: []string{"nc-*"}},
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

// Never mixing the two is what stops a user's choice and a node's claims from
// conflicting.
func TestTheFleetIsOwnNodesOrGrantedNodesButNeverAMixture(t *testing.T) {
	access := config.AccessConfig{NodeGrants: map[string][]string{
		"node-b": {"U1", "U3"},
	}}

	t.Run("a user with a node of their own never touches a granted one", func(t *testing.T) {
		host := newTestHost(t, nil, access)
		attachStubs(host,
			nodeStub{id: "node-a", owner: "U1"},
			nodeStub{id: "node-b", owner: "U2"},
		)
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

// Uses the real SQLite store, opened twice: an in-memory fake would pass
// without the pin ever reaching disk.
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

	first := newTestHost(t, openPins(), config.AccessConfig{})
	attachStubs(first, fleet...)
	initial, err := first.delegate(ctx, meta)
	if err != nil {
		t.Fatalf("first election: %v", err)
	}

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

// A pin left pointing at a node that is gone would re-elect on every turn,
// moving the conversation each time.
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

	stored, found, err := pins.Get(context.Background(), ref)
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if stored.NodeID != second.node.nodeID {
		t.Fatalf("the stored pin still names %q; it should name %q", stored.NodeID, second.node.nodeID)
	}

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

// The conversation key leaves out the user on purpose, so a second speaker in
// a channel stays on the first speaker's node.
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
	second, err := host.delegate(ctx, agent.SessionMetadata{ChannelID: "C1", ThreadTS: "1.1", UserID: "U2"})
	if err != nil {
		t.Fatalf("U2's turn: %v", err)
	}
	if second.node.nodeID != "node-a" {
		t.Fatalf("the second speaker moved the conversation to %q", second.node.nodeID)
	}
}

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

// A database hiccup must not look to the user like the agent forgot the
// conversation.
func TestAnUnreadablePinFailsTheTurnRatherThanMovingIt(t *testing.T) {
	boom := errors.New("the pin store is down")
	host := newTestHost(t, &memPins{getErr: boom}, config.AccessConfig{})
	attachStubs(host, nodeStub{id: "node-a", owner: "U1"}, nodeStub{id: "node-b", owner: "U1"})
	ctx := agent.WithConversation(context.Background(), agent.ConversationKey{ChannelID: "C1"})

	if _, err := host.delegate(ctx, agent.SessionMetadata{ChannelID: "C1", UserID: "U1"}); !errors.Is(err, boom) {
		t.Fatalf("want the store's error, got %v", err)
	}
}

// The node is already chosen, so refusing the turn over bookkeeping would be
// the wrong trade.
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

// During a credential rotation one machine holds two connections; listing it
// twice would give it double its share.
func TestARotatingNodeIsNotRoundRobinnedAgainstItself(t *testing.T) {
	host := newTestHost(t, nil, config.AccessConfig{})
	attachStubs(host,
		nodeStub{id: "node-a", owner: "U1"},
		nodeStub{id: "node-b", owner: "U1"},
	)
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

// A separate user message after a tool result is the empty-reply bug
// (assertNoConsecutiveUserAfterTool), so the notice must be in the prompt text.
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
	if got.History != req.History {
		t.Fatal("the notice was folded into the thread transcript instead of the user message")
	}
	if strings.Contains(got.Text, "<context>") {
		t.Fatal("the notice reused the tag the backends already emit")
	}
	if !strings.Contains(got.Text, "node-a") {
		t.Fatal("the notice does not say where the conversation came from")
	}

	only := foldTakeover(agent.PromptRequest{}, "")
	if strings.TrimSpace(only.Text) != only.Text || !strings.HasPrefix(only.Text, "<"+takeoverTag+">") {
		t.Fatalf("an empty prompt produced a malformed message: %q", only.Text)
	}
}

// A model told on every message that it just arrived and can see nothing
// behaves as though that were true.
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
	other := host.preparePrompt("session-2", agent.PromptRequest{Text: "hello"})
	if other.Text != "hello" {
		t.Fatalf("an untouched prompt was modified: %q", other.Text)
	}
}

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
	_, err := host.sessionNode("s1")
	if !errors.Is(err, agent.ErrSessionGone) {
		t.Fatalf("a session whose node left gave %v, want ErrSessionGone", err)
	}
	if errors.Is(err, ErrNoNode) {
		t.Fatal("a lost session was reported as a gateway with no nodes")
	}
}
