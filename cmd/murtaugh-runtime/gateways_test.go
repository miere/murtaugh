package main

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/nodesocket"
)

// This file is #197's node half. Three things are asserted here because three
// things go wrong without them, and two of the three only go wrong days later:
// a node that never hops sits on a standby, a node that replaces its seed cannot
// come home, and a fleet that redials in lockstep turns one failover into a
// second outage.

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

const seed = "wss://seed.example.com:8787"

// TestARedirectMovesTheNodeToTheNamedGatewayAtOnce is the node's half of the
// redirect test.
//
// "At once" is the part worth pinning. A redirect is not a failure — the fleet
// answered and named the leader — so inheriting a backoff earned by unrelated
// failures would leave a healthy node idle for up to half a minute holding the
// address it needs.
func TestARedirectMovesTheNodeToTheNamedGatewayAtOnce(t *testing.T) {
	leader := "wss://leader.example.com:8787"
	gateways := newGatewayList(seed)

	hop := gateways.refusal(seed, &nodesocket.RedirectError{
		Endpoint:  seed,
		Addresses: []string{leader},
	}, quietLogger())
	if !hop {
		t.Fatal("a redirect naming a leader did not produce a hop")
	}
	if got := gateways.current(); got != leader {
		t.Fatalf("after the redirect the node dials %q, want %q", got, leader)
	}
}

// TestLearnedGatewaysAugmentTheSeedAndNeverReplaceIt is #197's stale-seed
// recovery test.
//
// The failure it prevents cannot be recovered from in the field: a node that
// overwrote its seed with a learned address, and was then asleep through a
// topology change, wakes holding only addresses that no longer exist and no way
// to reach anything. So the seed is permanent and every cycle returns to it —
// which is also what makes a learned address that has gone away cost one round
// of backoff rather than the node's whole life.
func TestLearnedGatewaysAugmentTheSeedAndNeverReplaceIt(t *testing.T) {
	leader := "wss://leader.example.com:8787"
	gateways := newGatewayList(seed)
	gateways.refusal(seed, &nodesocket.RedirectError{Endpoint: seed, Addresses: []string{leader}}, quietLogger())

	if got := gateways.current(); got != leader {
		t.Fatalf("dialling %q, want the learned leader %q", got, leader)
	}
	// The learned gateway is now gone: the machine it named was retired between
	// the redirect and this dial.
	gateways.refusal(leader, errNoAnswer(), quietLogger())
	if got := gateways.current(); got != seed {
		t.Fatalf("with the learned gateway gone the node dials %q, want the seed %q", got, seed)
	}

	// And it keeps coming back to the seed for as long as nothing answers,
	// rather than settling on the dead address.
	for range 4 {
		address := gateways.current()
		gateways.refusal(address, errNoAnswer(), quietLogger())
	}
	if got := gateways.current(); got != seed && got != leader {
		t.Fatalf("the node wandered off its known addresses: %q", got)
	}
	if !gateways.known(seed) {
		t.Fatal("the seed was evicted")
	}
}

// TestARejectedCredentialIsNotConfusedWithADeadGateway is the distinction #197
// exists for. All three of these are "the node did not attach", and a node that
// reports them identically leaves whoever reads the log with no idea whether to
// look at the network, the fleet, or the token.
func TestARejectedCredentialIsNotConfusedWithADeadGateway(t *testing.T) {
	for name, err := range map[string]error{
		"credential": nodesocket.ErrCredentialRejected,
		"unelected":  nodesocket.ErrGatewayUnavailable,
		"unreachable": errors.Join(nodesocket.ErrGatewayUnreachable,
			errors.New("connect: connection refused")),
	} {
		gateways := newGatewayList(seed)
		if hop := gateways.refusal(seed, err, quietLogger()); hop {
			t.Errorf("%s: produced a hop; only a redirect names somewhere to go", name)
		}
	}

	// And a redirect is the only one that does.
	gateways := newGatewayList(seed)
	if hop := gateways.refusal(seed, &nodesocket.RedirectError{
		Endpoint:  seed,
		Addresses: []string{"wss://leader.example.com:8787"},
	}, quietLogger()); !hop {
		t.Error("a redirect did not produce a hop")
	}
}

// TestAnAddressTheGatewayOffersStillObeysTheTransportRule keeps the redirect
// from becoming a way to talk a node into cleartext.
//
// An address that arrived over an unauthenticated handshake response is LESS
// trustworthy than one an operator typed, so "a gateway told me to" is not a
// reason to dial something the wss rule refuses.
func TestAnAddressTheGatewayOffersStillObeysTheTransportRule(t *testing.T) {
	gateways := newGatewayList(seed)
	hop := gateways.refusal(seed, &nodesocket.RedirectError{
		Endpoint:  seed,
		Addresses: []string{"ws://leader.example.com:8787"},
	}, quietLogger())
	if hop {
		t.Fatal("the node hopped to a plain ws:// address on a real network")
	}
	if gateways.known("ws://leader.example.com:8787") {
		t.Fatal("an unusable address was learned")
	}
	if got := gateways.current(); got != seed {
		t.Fatalf("dialling %q, want the seed %q", got, seed)
	}
}

// TestARedirectLoopStopsSpinning bounds the one shape this design cannot fix:
// two gateways that each name the other. Following that chain without a wait
// pegs two CPUs and fills two logs.
func TestARedirectLoopStopsSpinning(t *testing.T) {
	a, b := "wss://a.example.com:8787", "wss://b.example.com:8787"
	gateways := newGatewayList(a)
	for range maxConsecutiveHops {
		if hop := gateways.refusal(gateways.current(), &nodesocket.RedirectError{
			Addresses: []string{a, b},
		}, quietLogger()); !hop {
			t.Fatal("stopped hopping before the budget was spent")
		}
	}
	if hop := gateways.refusal(gateways.current(), &nodesocket.RedirectError{
		Addresses: []string{a, b},
	}, quietLogger()); hop {
		t.Fatal("a redirect loop kept hopping with no wait between attempts")
	}

	// The budget is per chain, not per process. Having paid a backoff there is
	// no hot loop left to stop, and a node that spent its budget once must not
	// spend the rest of its life refusing to follow a redirect — it would then
	// find every later failover only by working through its address list one
	// backoff at a time.
	gateways.waited()
	if hop := gateways.refusal(gateways.current(), &nodesocket.RedirectError{
		Addresses: []string{a, b},
	}, quietLogger()); !hop {
		t.Fatal("the hop budget was never returned after the node backed off")
	}
}

// TestJitterSpreadsAFleetsReconnections is #197's thundering-herd test.
//
// Every node in a fleet discovers a dead gateway at the same instant, so an
// unjittered backoff has them all redial together — repeatedly, since they all
// fail together too. The new leader's first act is then to absorb the whole
// fleet's handshakes at once, which is how a failover that worked becomes a
// second outage.
//
// Two properties are what matter, and neither is "the numbers look random":
// every wait is inside (d/2, d], so no node ever waits longer than the schedule
// says or hammers immediately; and a fleet's waits differ, so they arrive spread
// across that window.
func TestJitterSpreadsAFleetsReconnections(t *testing.T) {
	const fleet = 200
	window := reconnectCeiling

	seen := make(map[time.Duration]int, fleet)
	for range fleet {
		wait := jitter(window)
		if wait < window/2 || wait > window {
			t.Fatalf("jitter(%v) = %v, outside the half-window it must stay in", window, wait)
		}
		seen[wait]++
	}
	// A fleet of two hundred landing on a handful of instants is a fleet that
	// still arrives together.
	if len(seen) < fleet/2 {
		t.Fatalf("%d nodes produced only %d distinct waits; they would still arrive in a clump", fleet, len(seen))
	}

	// The floor is jittered too. The first retry after a drop is the one every
	// node in the fleet makes simultaneously, so it is the one that matters
	// most — and it is the one a "jitter only the long waits" shortcut misses.
	first := make(map[time.Duration]int, fleet)
	for range fleet {
		first[jitter(reconnectFloor)]++
	}
	if len(first) < 2 {
		t.Fatalf("the first retry is not jittered: %d nodes produced one wait", fleet)
	}
}

func errNoAnswer() error {
	return errors.Join(nodesocket.ErrGatewayUnreachable, errors.New("dial tcp: i/o timeout"))
}
