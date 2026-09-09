package app

import (
	"context"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

// The gateway's inbound node listener and the election are built in different
// places and meet in exactly three lines, all of them here. Every one of the
// three was deletable with nodehost, election and cmd/... all green, because
// those packages are tested in isolation and nothing exercised the file that
// joins them.
//
// What each deletion costs, which is why this file exists at all:
//
//   - Follow — the Host defaults to refusing, so a gateway started with
//     -node-listen accepts no node, ever, for the life of the process.
//   - Detach — a demoted gateway keeps every attached node holding a socket
//     nothing will route a conversation over, which is the failure #197 exists
//     to remove.
//   - Address — every standby answers "the elected gateway accepts no runtime
//     nodes" forever, silently degrading failover to seed-only backoff.
//
// The two ends already have tests. These are about the join.

// recordingEndpoint is a stand-in for cmd/murtaugh-gateway's listener that
// records the three calls instead of binding a port.
type recordingEndpoint struct {
	address  config.LeaderAddress
	followed []LeaderView
	detached []string
}

func (e *recordingEndpoint) endpoint() NodeEndpoint {
	return NodeEndpoint{
		Address: func() config.LeaderAddress { return e.address },
		Follow:  func(v LeaderView) { e.followed = append(e.followed, v) },
		Detach:  func(reason string) { e.detached = append(e.detached, reason) },
	}
}

// idleLocker is a Locker that is never contended. The election wiring never
// touches the store in this file — promotion is the gateway's business and has
// its own tests — so every method here is the "nothing happened" answer.
type idleLocker struct{}

func (idleLocker) Acquire(context.Context) (config.Lease, bool, error) {
	return config.Lease{}, false, nil
}

func (idleLocker) Renew(_ context.Context, lease config.Lease) (config.Lease, bool, error) {
	return lease, false, nil
}
func (idleLocker) Verify(context.Context, config.Lease) (bool, error) { return false, nil }
func (idleLocker) Release(context.Context, config.Lease) error        { return nil }
func (idleLocker) Publish(context.Context, config.Lease, config.LeaderAddress) error {
	return nil
}
func (idleLocker) Holder(context.Context) (config.Lease, bool, error) {
	return config.Lease{}, false, nil
}
func (idleLocker) TTL() time.Duration { return 30 * time.Second }
func (idleLocker) Backend() string    { return "test" }
func (idleLocker) Close() error       { return nil }

// wiringTestApp builds the composition root with the node listener attached,
// exactly as cmd/murtaugh-gateway attaches it.
func wiringTestApp(endpoint *recordingEndpoint) (*Application, *gatewayHolder) {
	a := &Application{logger: quietLogger()}
	a.WithNodeEndpoint(endpoint.endpoint())
	holder := &gatewayHolder{}
	// A gateway that never served: StopServing on it is a documented no-op, so
	// the demotion path below reaches the node endpoint without a Slack socket.
	holder.swap(a.buildGateway(config.Config{}))
	return a, holder
}

// TestTheNodeListenerFollowsTheElection is the Follow wiring.
//
// Without it the Host's leadership stays nil, `leading` answers false for every
// handshake, and a gateway started with -node-listen turns away every node for
// the life of the process — with no error anywhere, because refusing is the
// Host's deliberate default until an election is installed.
func TestTheNodeListenerFollowsTheElection(t *testing.T) {
	endpoint := &recordingEndpoint{}
	a, holder := wiringTestApp(endpoint)

	runner, err := a.followElection(idleLocker{}, holder)
	if err != nil {
		t.Fatalf("followElection: %v", err)
	}
	if len(endpoint.followed) != 1 {
		t.Fatalf("the node listener was handed the election %d times, want exactly 1: "+
			"a gateway started with -node-listen accepts no node until it is", len(endpoint.followed))
	}
	if endpoint.followed[0] != LeaderView(runner) {
		t.Errorf("the node listener follows %v, want the election runner this gateway contends with", endpoint.followed[0])
	}
}

// TestDemotionDetachesTheNodeListener is the Detach wiring.
//
// TestDemotionDropsItsNodesAndJournalsTheDrop in internal/nodehost calls
// DetachAll directly, so it proves the method works and says nothing about
// whether demotion calls it. This says demotion calls it.
func TestDemotionDetachesTheNodeListener(t *testing.T) {
	endpoint := &recordingEndpoint{}
	a, holder := wiringTestApp(endpoint)

	opts := a.electionOptions(idleLocker{}, holder)
	if opts.Callbacks.OnDemote == nil {
		t.Fatal("the election was built with no demote callback")
	}
	opts.Callbacks.OnDemote(context.Background(), "the leader lock was taken by another node")

	if len(endpoint.detached) != 1 {
		t.Fatalf("demotion dropped attached nodes %d times, want exactly 1: "+
			"a node cannot discover for itself that its gateway stood down", len(endpoint.detached))
	}
	if endpoint.detached[0] != "the leader lock was taken by another node" {
		t.Errorf("the drop was given reason %q; the journalled reason is the only trace a nightly drop leaves", endpoint.detached[0])
	}
}

// TestTheElectionPublishesWhereNodesReachThisGateway is the Address wiring.
//
// Without it the lock record carries no address, so every standby in the fleet
// answers a node with "the elected gateway accepts no runtime nodes" — forever,
// and correctly as far as the node can tell. Failover degrades to each node
// working through its own seed a backoff at a time, which is exactly the state
// #197 exists to replace.
func TestTheElectionPublishesWhereNodesReachThisGateway(t *testing.T) {
	endpoint := &recordingEndpoint{
		address: config.LeaderAddress{"wss://gateway.example.com:8787", "wss://192.0.2.10:8787"},
	}
	a, holder := wiringTestApp(endpoint)

	opts := a.electionOptions(idleLocker{}, holder)
	if opts.Address == nil {
		t.Fatal("the election was built with no address function; the lock record would never name this gateway")
	}
	if got := opts.Address(); !got.Equal(endpoint.address) {
		t.Errorf("the election publishes %v, want the listener's own address %v", got, endpoint.address)
	}

	// And it is asked on every tick rather than captured once, because the
	// listener may still be binding when the election promotes and a laptop's
	// routable address changes when it changes network.
	endpoint.address = config.LeaderAddress{"wss://gateway.example.com:9999"}
	if got := opts.Address(); !got.Equal(endpoint.address) {
		t.Errorf("after the address moved the election still publishes %v, want %v", got, endpoint.address)
	}
}

// TestAGatewayWithNoNodeListenerStillElects is the shipping default: every
// gateway today opens no node listener, so all three wirings are nil and the
// election must be built anyway.
func TestAGatewayWithNoNodeListenerStillElects(t *testing.T) {
	a := &Application{logger: quietLogger()}
	holder := &gatewayHolder{}
	holder.swap(a.buildGateway(config.Config{}))

	runner, err := a.followElection(idleLocker{}, holder)
	if err != nil || runner == nil {
		t.Fatalf("followElection with no node endpoint: runner=%v err=%v", runner, err)
	}
	opts := a.electionOptions(idleLocker{}, holder)
	if opts.Address != nil {
		t.Error("a gateway with no node listener offered an address; a standby would redirect nodes to a door it cannot open")
	}
	// Demotion must still stand the Slack side down.
	opts.Callbacks.OnDemote(context.Background(), "shutting down")
}
