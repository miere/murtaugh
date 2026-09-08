package nodesocket

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The dialler's refusals are the compensating control for deferring TLS to
// item 11. #170 makes wss mandatory; nodehost.Listen deliberately does not
// provide it, and the sentence that makes that acceptable is "the dialler
// enforces the other half of this: it refuses plain ws:// to anything but a
// loopback host". A relaxation or a reordering here ships a node's bearer
// token — which travels in an Authorization header — in cleartext across a
// real network, so it is pinned rather than trusted to a comment.

func TestPlainWebSocketIsOnlyAllowedToALoopbackHost(t *testing.T) {
	// Loopback is carved out because that is the `--role both` deployment the
	// split is exercised with: there is no network for anything to intercept.
	for _, address := range []string{
		"ws://127.0.0.1:9000",
		"ws://localhost:9000",
		"ws://[::1]:9000",
		// The scheme that carries the credential safely is allowed anywhere,
		// which is the whole point of the restriction being on ws:// alone.
		"wss://gateway.example.com",
		"wss://gateway.example.com:9000",
	} {
		if _, err := resolveEndpoint(address); err != nil {
			t.Errorf("resolveEndpoint(%q) = %v, want it accepted", address, err)
		}
	}

	for _, address := range []string{
		"ws://gateway.example.com:9000",
		"ws://192.0.2.10:9000",
		"ws://10.1.2.3",
		"ws://[2001:db8::1]:9000",
		// Not loopback, whatever the name suggests: only 127/8, ::1 and the
		// literal "localhost" are.
		"ws://localhost.example.com:9000",
	} {
		endpoint, err := resolveEndpoint(address)
		if err == nil {
			t.Errorf("resolveEndpoint(%q) = %q with no error; a node credential would cross a real network in cleartext", address, endpoint)
			continue
		}
		// The operator has to be told what to change, not merely that something
		// was refused.
		if !strings.Contains(err.Error(), "wss://") {
			t.Errorf("resolveEndpoint(%q) = %v, which does not say what to use instead", address, err)
		}
	}
}

// A scheme that is neither ws:// nor wss:// is refused rather than defaulted.
// An operator who pasted the gateway's https:// address must be told, not
// silently upgraded to something that may not be what they meant.
func TestOnlyTheTwoWebSocketSchemesAreDialled(t *testing.T) {
	for _, address := range []string{
		"https://gateway.example.com",
		"http://127.0.0.1:9000",
		"gateway.example.com:9000",
		"",
		"   ",
	} {
		if endpoint, err := resolveEndpoint(address); err == nil {
			t.Errorf("resolveEndpoint(%q) = %q with no error", address, endpoint)
		}
	}
}

// The endpoint path is supplied here so an operator configures a host and not a
// URL shape — but a path they did supply is theirs, because that is what a
// reverse proxy in front of the gateway needs.
func TestTheEndpointPathIsSuppliedButNotOverridden(t *testing.T) {
	for address, want := range map[string]string{
		"wss://gateway.example.com":       "wss://gateway.example.com" + Path,
		"wss://gateway.example.com/":      "wss://gateway.example.com" + Path,
		"ws://127.0.0.1:9000":             "ws://127.0.0.1:9000" + Path,
		"wss://gateway.example.com/proxy": "wss://gateway.example.com/proxy",
	} {
		got, err := resolveEndpoint(address)
		if err != nil {
			t.Errorf("resolveEndpoint(%q): %v", address, err)
			continue
		}
		if got != want {
			t.Errorf("resolveEndpoint(%q) = %q, want %q", address, got, want)
		}
	}
}

// Dial refuses the plaintext address before it opens anything. The distinction
// matters: a refusal that happened after the TCP connect would mean the guard
// had already been passed by something.
func TestDialRefusesAPlaintextCredentialWithoutTouchingTheNetwork(t *testing.T) {
	// TEST-NET-1: routable to nothing, so a dial that did happen would hang
	// until this context expires rather than failing fast for another reason.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Dial(ctx, "ws://192.0.2.10:9000", DialOptions{Token: "node-secret"})

	if err == nil {
		t.Fatal("a node token was carried in cleartext to a non-loopback host")
	}
	if !strings.Contains(err.Error(), "wss://") {
		t.Fatalf("Dial refused with %v, which does not name the scheme required", err)
	}
	if strings.Contains(err.Error(), "nodesocket: dial") {
		t.Fatalf("Dial reached the network before refusing: %v", err)
	}
}

// An empty token is refused before anything is opened too. There is no
// unauthenticated connection this transport can reach, so dialling one and
// letting the gateway answer 401 would be a connection attempt made for a
// credential the node knows it does not have.
func TestDialRefusesAnEmptyTokenBeforeItConnects(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()

	var accepted atomic.Int64
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	address := "ws://" + listener.Addr().String()

	for _, token := range []string{"", "   "} {
		if _, err := Dial(ctx, address, DialOptions{Token: token}); err == nil {
			t.Fatalf("Dial with token %q was allowed", token)
		}
	}
	if n := accepted.Load(); n != 0 {
		t.Fatalf("a node with no credential opened %d connections to the gateway", n)
	}
}
