package nodesocket

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPlainWebSocketIsOnlyAllowedToALoopbackHost(t *testing.T) {
	for _, address := range []string{
		"ws://127.0.0.1:9000",
		"ws://localhost:9000",
		"ws://[::1]:9000",
		"wss://gateway.example.com",
		"wss://gateway.example.com:9000",
	} {
		if _, err := ResolveEndpoint(address); err != nil {
			t.Errorf("ResolveEndpoint(%q) = %v, want it accepted", address, err)
		}
	}

	for _, address := range []string{
		"ws://gateway.example.com:9000",
		"ws://192.0.2.10:9000",
		"ws://10.1.2.3",
		"ws://[2001:db8::1]:9000",
		"ws://localhost.example.com:9000",
	} {
		endpoint, err := ResolveEndpoint(address)
		if err == nil {
			t.Errorf("ResolveEndpoint(%q) = %q with no error; a node credential would cross a real network in cleartext", address, endpoint)
			continue
		}
		if !strings.Contains(err.Error(), "wss://") {
			t.Errorf("ResolveEndpoint(%q) = %v, which does not say what to use instead", address, err)
		}
	}
}

// An operator who pasted the gateway's https:// address must be told, not silently switched to a
// scheme they may not have meant.
func TestOnlyTheTwoWebSocketSchemesAreDialled(t *testing.T) {
	for _, address := range []string{
		"https://gateway.example.com",
		"http://127.0.0.1:9000",
		"gateway.example.com:9000",
		"",
		"   ",
	} {
		if endpoint, err := ResolveEndpoint(address); err == nil {
			t.Errorf("ResolveEndpoint(%q) = %q with no error", address, endpoint)
		}
	}
}

// A path the operator supplied is kept, because a reverse proxy in front of the gateway may need it.
func TestTheEndpointPathIsSuppliedButNotOverridden(t *testing.T) {
	for address, want := range map[string]string{
		"wss://gateway.example.com":       "wss://gateway.example.com" + Path,
		"wss://gateway.example.com/":      "wss://gateway.example.com" + Path,
		"ws://127.0.0.1:9000":             "ws://127.0.0.1:9000" + Path,
		"wss://gateway.example.com/proxy": "wss://gateway.example.com/proxy",
	} {
		got, err := ResolveEndpoint(address)
		if err != nil {
			t.Errorf("ResolveEndpoint(%q): %v", address, err)
			continue
		}
		if got != want {
			t.Errorf("ResolveEndpoint(%q) = %q, want %q", address, got, want)
		}
	}
}

// A refusal after the TCP connect would mean something had already got past the guard.
func TestDialRefusesAPlaintextCredentialWithoutTouchingTheNetwork(t *testing.T) {
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
