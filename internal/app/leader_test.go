package app

import (
	"context"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

type recordingEndpoint struct {
	address   config.LeaderAddress
	followed  []LeaderView
	detached  []string
	onboarded []NodeOnboarding
}

func (e *recordingEndpoint) endpoint() NodeEndpoint {
	return NodeEndpoint{
		Address: func() config.LeaderAddress { return e.address },
		Follow:  func(v LeaderView) { e.followed = append(e.followed, v) },
		Detach:  func(reason string) { e.detached = append(e.detached, reason) },
		Onboard: func(o NodeOnboarding) { e.onboarded = append(e.onboarded, o) },
	}
}

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

func wiringTestApp(endpoint *recordingEndpoint) (*Application, *gatewayHolder) {
	a := &Application{logger: quietLogger()}
	a.WithNodeEndpoint(endpoint.endpoint())
	holder := &gatewayHolder{}
	holder.swap(a.buildGateway(config.Config{}))
	return a, holder
}

// Without the Follow wiring a -node-listen gateway silently refuses every node, because
// refusing is the Host's default until an election is installed.
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

// The nodehost test calls DetachAll directly; only this one proves demotion calls it.
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

// Without the Address wiring every standby tells nodes the leader accepts none, and
// failover silently degrades to each node backing off against its own seed.
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

	endpoint.address = config.LeaderAddress{"wss://gateway.example.com:9999"}
	if got := opts.Address(); !got.Equal(endpoint.address) {
		t.Errorf("after the address moved the election still publishes %v, want %v", got, endpoint.address)
	}
}

// An offer with no withdrawal traps the admin, because the gateway checks the node
// branch before the admin branch.
func TestTheNodeRegistryIsHandedBothEndsOfTheOnboardingTrigger(t *testing.T) {
	endpoint := &recordingEndpoint{}
	a, holder := wiringTestApp(endpoint)

	a.wireNodeOnboarding(holder)

	if len(endpoint.onboarded) != 1 {
		t.Fatalf("the node registry was handed the gateway's answers %d times, want exactly 1", len(endpoint.onboarded))
	}
	o := endpoint.onboarded[0]
	if o.References == nil {
		t.Error("References is nil: the gateway's agent names never reach the connect-time check")
	}
	if o.Unconfigured == nil {
		t.Error("Unconfigured is nil: a node that attaches with nothing configured is never onboarded")
	}
	if o.Settled == nil {
		t.Error("Settled is nil: an invitation outlives its node for the life of the process, " +
			"and an admin who once plugged one in cannot reach their own gateway's form again")
	}
	holder.swap(a.buildGateway(config.Config{}))
	o.Unconfigured(context.Background(), "node-7", "U0OWNER01")
	o.Settled("node-7", "U0OWNER01")
}

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
	opts.Callbacks.OnDemote(context.Background(), "shutting down")
}
