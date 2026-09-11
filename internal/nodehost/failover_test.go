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

type elected struct{}

func (elected) Allow(context.Context) bool { return true }
func (elected) Leader(context.Context) (config.LeaderAddress, bool) {
	return config.LeaderAddress{"ws://127.0.0.1:1/murtaugh/node/link"}, true
}

type standby struct{ leader config.LeaderAddress }

func (standby) Allow(context.Context) bool { return false }
func (s standby) Leader(context.Context) (config.LeaderAddress, bool) {
	return s.leader, true
}

type leaderless struct{}

func (leaderless) Allow(context.Context) bool { return false }
func (leaderless) Leader(context.Context) (config.LeaderAddress, bool) {
	return nil, false
}

func refusedHost(t *testing.T, leadership nodehost.Leadership) (address, token string) {
	t.Helper()
	return refusedHostLogging(t, leadership, testLogger())
}

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

// A close frame cannot carry the address: the transport turns every close code
// into EOF, so the node would redial the same standby forever.
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
	if len(redirect.Addresses) != 2 ||
		redirect.Addresses[0] != leader[0] ||
		redirect.Addresses[1] != leader[1] {
		t.Fatalf("the redirect did not carry the leader's addresses in order: %v", redirect.Addresses)
	}
}

// Accepting with no election wired would let a standby hold a node whose
// conversations go to the real leader.
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

// The node logs this like the harmless "nobody elected yet", but it never
// heals, so the gateway has to log it at ERROR.
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

// "Nobody leads yet" means wait and "wrong gateway" means hop; mixing them up
// makes a node hop to nowhere or sit still.
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

// A leader started without -node-listen takes no nodes; the standby must say
// so rather than redirect to a blank address or to itself.
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

// A redirect sent before the credential check would tell anyone who can reach
// the port where the serving gateway is.
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

// Behind a TLS proxy the gateway cannot discover its public name; what it can
// discover is the wrong host and port.
func TestTheAdvertisedAddressReplacesTheDiscoveredOne(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	host, err := nodehost.New(nodehost.Options{
		Tokens:    &memTokens{records: map[string]config.NodeToken{}},
		Logger:    testLogger(),
		Advertise: "gateway.example.com, wss://192.0.2.10:8443",
	})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	if got := host.Address(); len(got) != 0 {
		t.Fatalf("Address() before listening = %v, want nothing", got)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = host.Serve(ctx, listener) }()

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

	cancel()
	waitFor(t, "the address to be withdrawn when the listener stops", func() bool {
		return len(host.Address()) == 0
	})
}
