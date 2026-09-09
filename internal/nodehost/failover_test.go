package nodehost_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/nodehost"
	"github.com/miere/murtaugh/internal/nodesocket"
	"github.com/miere/murtaugh/internal/nodetoken"
)

// This file is #197's gateway half: only the elected gateway accepts nodes, and
// a gateway that is not elected says where the elected one is rather than
// dropping the connection.
//
// The three refusals below are three different sentences on purpose. A node that
// cannot tell "wrong gateway" from "gateway down" from "credential rejected"
// produces a support ticket with no evidence in it, which is the failure #197
// exists to remove — not the reconnect, which already worked.

// elected is a gateway that holds the lock.
type elected struct{}

func (elected) Allow(context.Context) bool { return true }
func (elected) Leader(context.Context) (config.LeaderAddress, bool) {
	return config.LeaderAddress{"ws://127.0.0.1:1/murtaugh/node/link"}, true
}

// standby is a gateway that does not hold the lock but can read who does.
type standby struct{ leader config.LeaderAddress }

func (standby) Allow(context.Context) bool { return false }
func (s standby) Leader(context.Context) (config.LeaderAddress, bool) {
	return s.leader, true
}

// leaderless is a gateway that does not lead and finds no live claim: the lock
// is released or lapsed and nobody has taken it yet.
type leaderless struct{}

func (leaderless) Allow(context.Context) bool { return false }
func (leaderless) Leader(context.Context) (config.LeaderAddress, bool) {
	return nil, false
}

// refusedHost stands up a Host with the given leadership and returns its
// address and a valid credential for it. Everything about the node is real
// except that it never gets to attach.
func refusedHost(t *testing.T, leadership nodehost.Leadership) (address, token string) {
	t.Helper()
	return refusedHostLogging(t, leadership, testLogger())
}

// refusedHostLogging is refusedHost with the gateway's own log captured, for the
// one refusal whose evidence is on this side rather than the node's.
func refusedHostLogging(t *testing.T, leadership nodehost.Leadership, logger *slog.Logger) (address, token string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	store := &memTokens{records: map[string]config.NodeToken{}}
	minted := mintInto(t, store, "node-1")
	host, err := nodehost.New(nodehost.Options{Tokens: store, Logger: logger})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if leadership != nil {
		host.FollowLeader(leadership)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = host.Serve(ctx, listener) }()
	return "ws://" + listener.Addr().String(), minted.Token
}

// TestAStandbyRedirectsANodeToTheElectedGateway is #197's redirect test.
//
// The node is told where to go, in a form it can act on, rather than being
// dropped. A close frame could not carry this: the transport collapses every
// ordinary close code to EOF and discards the reason, so the node would redial
// the same standby forever.
func TestAStandbyRedirectsANodeToTheElectedGateway(t *testing.T) {
	leader := config.LeaderAddress{"wss://gateway.example.com:8787", "wss://192.0.2.10:8787"}
	address, token := refusedHost(t, standby{leader: leader})

	_, err := nodesocket.Dial(context.Background(), address, nodesocket.DialOptions{Token: token})
	if err == nil {
		t.Fatal("a standby accepted a node")
	}
	var redirect *nodesocket.RedirectError
	if !errors.As(err, &redirect) {
		t.Fatalf("a standby's refusal did not reach the node as a redirect: %v", err)
	}
	// BOTH forms, in order. An IP alone is not enough — DHCP, a changed network
	// and a VPN all move it — and a hostname alone does not resolve from every
	// network a node wakes up on.
	if len(redirect.Addresses) != 2 ||
		redirect.Addresses[0] != leader[0] ||
		redirect.Addresses[1] != leader[1] {
		t.Fatalf("the redirect did not carry the leader's addresses in order: %v", redirect.Addresses)
	}
}

// TestAGatewayWithNoElectionAcceptsNothing pins the default that the doc comment
// has claimed since item 7 and nothing enforced until now.
//
// The two ways to get this wrong are not symmetric. A Host that accepts because
// nobody wired an election is a standby holding a node that believes all is
// well, while its conversations go to the gateway that actually leads. So it
// refuses — and says so where it can be seen, which is the test below.
func TestAGatewayWithNoElectionAcceptsNothing(t *testing.T) {
	address, token := refusedHost(t, nil)

	_, err := nodesocket.Dial(context.Background(), address, nodesocket.DialOptions{Token: token})
	if err == nil {
		t.Fatal("a Host with no election accepted a node")
	}
	if !errors.Is(err, nodesocket.ErrGatewayUnavailable) {
		t.Fatalf("refusal = %v, want it reported as a gateway that is not serving", err)
	}
}

// TestAGatewayWithNoElectionSaysSoInItsOwnLog is where the un-wired case
// becomes visible, and it has to be here because it cannot be anywhere else.
//
// The node is answered with the same 503 as "nobody is elected yet" — there is
// nothing different for it to do — and it logs both with the same WARN line,
// "no gateway may be elected yet", which reads as the benign self-healing
// state. But only one of the two heals: a listener with no election wired
// refuses every node for the life of the process. So the gateway says it, at
// ERROR, and the ordinary unelected state stays quiet, or the loud line means
// nothing.
func TestAGatewayWithNoElectionSaysSoInItsOwnLog(t *testing.T) {
	unwired := &recordedLog{}
	address, token := refusedHostLogging(t, nil, unwired.logger())
	if _, err := nodesocket.Dial(context.Background(), address,
		nodesocket.DialOptions{Token: token}); err == nil {
		t.Fatal("a Host with no election accepted a node")
	}
	if !unwired.sawError() {
		t.Fatalf("a gateway that will accept no node for the life of the process logged nothing an operator could act on:\n%s", unwired.text())
	}
	if !strings.Contains(unwired.text(), "no leader election") {
		t.Errorf("the refusal does not name the missing election:\n%s", unwired.text())
	}

	// The contrast, which is what makes the line above worth anything: a fleet
	// that simply has no leader yet is an ordinary, temporary state and must not
	// produce the same alarm.
	settling := &recordedLog{}
	address, token = refusedHostLogging(t, leaderless{}, settling.logger())
	if _, err := nodesocket.Dial(context.Background(), address,
		nodesocket.DialOptions{Token: token}); err == nil {
		t.Fatal("a gateway with no elected leader accepted a node")
	}
	if settling.sawError() {
		t.Errorf("an election that has not settled yet was reported as a fault:\n%s", settling.text())
	}
}

// recordedLog captures a Host's own log. The Host writes from the goroutine
// serving the handshake, so the buffer is guarded.
type recordedLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (r *recordedLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *recordedLog) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(r, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func (r *recordedLog) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func (r *recordedLog) sawError() bool { return strings.Contains(r.text(), "level=ERROR") }

// TestNoElectedGatewayIsNotAWrongGateway keeps the two apart. "Nobody leads yet"
// is a wait; "you are at the wrong one" is a hop. A node that conflated them
// would hop to an address it was never given, or sit still when it was.
func TestNoElectedGatewayIsNotAWrongGateway(t *testing.T) {
	address, token := refusedHost(t, leaderless{})

	_, err := nodesocket.Dial(context.Background(), address, nodesocket.DialOptions{Token: token})
	if err == nil {
		t.Fatal("a gateway with no elected leader accepted a node")
	}
	var redirect *nodesocket.RedirectError
	if errors.As(err, &redirect) {
		t.Fatalf("an unelected fleet was reported as a redirect to nowhere: %v", err)
	}
	if !errors.Is(err, nodesocket.ErrGatewayUnavailable) {
		t.Fatalf("refusal = %v, want it reported as a gateway that is not serving", err)
	}
}

// TestALeaderThatAcceptsNoNodesIsNotADeadGateway covers the state a gateway
// started without -node-listen is permanently in: it leads, and it takes no
// nodes. The standby must say that rather than redirect to a blank address —
// or, worse, to itself.
func TestALeaderThatAcceptsNoNodesIsNotADeadGateway(t *testing.T) {
	address, token := refusedHost(t, standby{leader: nil})

	_, err := nodesocket.Dial(context.Background(), address, nodesocket.DialOptions{Token: token})
	if err == nil {
		t.Fatal("a standby accepted a node")
	}
	var redirect *nodesocket.RedirectError
	if !errors.As(err, &redirect) {
		t.Fatalf("refusal = %v, want a redirect carrying no address", err)
	}
	if len(redirect.Addresses) != 0 {
		t.Fatalf("the leader accepts no nodes, but the node was sent to %v", redirect.Addresses)
	}
}

// TestABadCredentialIsRefusedBeforeLeadershipIsMentioned is the ordering that
// keeps the redirect from being an authentication oracle.
//
// serveLink answers an unknown credential with an undifferentiated 401 on
// purpose — telling a caller which of "unknown", "revoked" and "wrong secret"
// applies is free reconnaissance. A redirect emitted before that check would be
// worse: it would hand the location of the workspace's serving gateway to
// anybody who can reach the port.
func TestABadCredentialIsRefusedBeforeLeadershipIsMentioned(t *testing.T) {
	address, _ := refusedHost(t, standby{leader: config.LeaderAddress{"wss://gateway.example.com:8787"}})
	stranger, err := nodetoken.Mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	_, err = nodesocket.Dial(context.Background(), address, nodesocket.DialOptions{Token: stranger.Token})
	if err == nil {
		t.Fatal("a standby accepted an unknown credential")
	}
	if !errors.Is(err, nodesocket.ErrCredentialRejected) {
		t.Fatalf("refusal = %v, want the credential rejection", err)
	}
	var redirect *nodesocket.RedirectError
	if errors.As(err, &redirect) {
		t.Fatalf("an unauthenticated caller was told where the leader is: %v", redirect.Addresses)
	}
}

// TestDemotionDropsItsNodesAndJournalsTheDrop is the other half of the
// leadership gate, and the half a green suite would otherwise hide.
//
// Gating ACCEPT does nothing for a node that is already attached when its
// gateway stands down: it cannot ask whether the gateway it holds a socket to
// still leads, so it would sit there, connected and unreachable, while its
// conversations went to the machine that actually leads. Dropping it is what
// turns it back into a node that dials, and dialling is what finds the redirect.
//
// And the drop is JOURNALLED, not announced. A laptop sleeping at 18:00
// disconnects every evening; a nightly message trains the admin to ignore the
// one that matters.
func TestDemotionDropsItsNodesAndJournalsTheDrop(t *testing.T) {
	rec := &recordingJournal{}
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}), journalling(rec))

	rig.host.DetachAll("this gateway stood down")

	select {
	case <-rig.nodeStopped:
	case <-time.After(10 * time.Second):
		t.Fatal("a node kept its connection to a gateway that had stood down")
	}
	waitFor(t, "the registry to forget the dropped node", func() bool {
		_, ok := rig.host.Attached()
		return !ok
	})
	if !rec.has("detached") {
		t.Fatal("a node was dropped on demotion and the journal has no record of it")
	}
}

// TestTheAdvertisedAddressReplacesTheDiscoveredOne is #170's "a configured
// hostname overrides the auto-discovered value". A gateway behind a reverse
// proxy holding the certificate cannot discover its own public name, and what it
// can discover — its bind address — is the wrong host and the wrong port.
//
// It is also where #170's name-and-address pair survives into a deployment that
// can use it: the discovered pair is ws:// and a node refuses to dial that
// anywhere but loopback, so the only place both forms can really be offered is
// what an operator configures. Hence a list, taken verbatim.
func TestTheAdvertisedAddressReplacesTheDiscoveredOne(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	host, err := nodehost.New(nodehost.Options{
		Tokens: &memTokens{records: map[string]config.NodeToken{}},
		Logger: testLogger(),
		// A name and an address, and the name first: an IP moves under DHCP, a
		// changed network or a VPN, and a hostname does not resolve from every
		// network a node wakes up on.
		Advertise: "gateway.example.com, wss://192.0.2.10:8443",
	})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	// Nothing is published before the listener binds. A configured address is
	// not evidence that this process can accept anything: two gateways on one
	// machine both try for the node port and one of them loses.
	if got := host.Address(); len(got) != 0 {
		t.Fatalf("Address() before listening = %v, want nothing", got)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = host.Serve(ctx, listener) }()

	// Verbatim, both of them, in order. The bare host gains wss:// because a
	// node will not put its credential on a real network in cleartext — and
	// gains NOTHING else, in particular not the listener's port: the terminator
	// this option exists for answers on its own port, and completing the
	// hostname from the local bind would point the fleet at 8787 instead of 443.
	want := []string{"wss://gateway.example.com", "wss://192.0.2.10:8443"}
	waitFor(t, "the configured addresses to be offered once the listener is up", func() bool {
		got := host.Address()
		if len(got) != len(want) {
			return false
		}
		for i := range want {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	})

	// And forgotten when serving stops. An address that outlived its listener is
	// one every standby keeps redirecting nodes to.
	cancel()
	waitFor(t, "the address to be withdrawn when the listener stops", func() bool {
		return len(host.Address()) == 0
	})
}
