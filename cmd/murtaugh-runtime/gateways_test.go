package main

import (
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/nodesocket"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

const seed = "wss://seed.example.com:8787"

// A redirect is not a failure, so the node must not wait out a backoff before
// dialling the gateway it was sent to.
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

// A node that replaced its seed and then slept through a topology change would
// wake up holding only dead addresses, with no way back.
func TestLearnedGatewaysAugmentTheSeedAndNeverReplaceIt(t *testing.T) {
	leader := "wss://leader.example.com:8787"
	gateways := newGatewayList(seed)
	gateways.refusal(seed, &nodesocket.RedirectError{Endpoint: seed, Addresses: []string{leader}}, quietLogger())

	if got := gateways.current(); got != leader {
		t.Fatalf("dialling %q, want the learned leader %q", got, leader)
	}
	gateways.refusal(leader, errNoAnswer(), quietLogger())
	if got := gateways.current(); got != seed {
		t.Fatalf("with the learned gateway gone the node dials %q, want the seed %q", got, seed)
	}

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

// Whoever reads the log must be able to tell whether to look at the network, the
// fleet or the token.
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

	gateways := newGatewayList(seed)
	if hop := gateways.refusal(seed, &nodesocket.RedirectError{
		Endpoint:  seed,
		Addresses: []string{"wss://leader.example.com:8787"},
	}, quietLogger()); !hop {
		t.Error("a redirect did not produce a hop")
	}
}

// A redirect address arrives over an unauthenticated handshake, so it is less
// trustworthy than one an operator typed and must not unlock cleartext.
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

// Two gateways that each name the other would otherwise peg two CPUs and fill two logs.
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

	gateways.waited()
	if hop := gateways.refusal(gateways.current(), &nodesocket.RedirectError{
		Addresses: []string{a, b},
	}, quietLogger()); !hop {
		t.Fatal("the hop budget was never returned after the node backed off")
	}
}

// Without jitter the whole fleet redials a new leader at the same instant, which
// turns a working failover into a second outage.
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
	if len(seen) < fleet/2 {
		t.Fatalf("%d nodes produced only %d distinct waits; they would still arrive in a clump", fleet, len(seen))
	}

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
