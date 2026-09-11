package nodehost

import (
	"errors"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

func TestBothANameAndAnAddressAreOffered(t *testing.T) {
	got := discoverAddresses("0.0.0.0:8787",
		func() (string, error) { return "gateway.local", nil },
		func() string { return "192.0.2.10" })

	want := config.LeaderAddress{"ws://gateway.local:8787", "ws://192.0.2.10:8787"}
	if !got.Equal(want) {
		t.Fatalf("discovered %v, want the name first and the address behind it: %v", got, want)
	}
}

// The routing table would publish whichever interface reaches the internet,
// not the one this gateway listens on.
func TestASpecificBindIsPreferredToTheRoutingTable(t *testing.T) {
	got := discoverAddresses("192.0.2.7:8787",
		func() (string, error) { return "gateway.local", nil },
		func() string { return "198.51.100.1" })

	want := config.LeaderAddress{"ws://gateway.local:8787", "ws://192.0.2.7:8787"}
	if !got.Equal(want) {
		t.Fatalf("discovered %v, want the bound address rather than the routed one: %v", got, want)
	}
}

func TestOneHalfIsBetterThanNothingAndNoHalvesIsNothing(t *testing.T) {
	noName := discoverAddresses("0.0.0.0:8787",
		func() (string, error) { return "", errors.New("no hostname") },
		func() string { return "192.0.2.10" })
	if !noName.Equal(config.LeaderAddress{"ws://192.0.2.10:8787"}) {
		t.Fatalf("with no hostname, discovered %v", noName)
	}

	noRoute := discoverAddresses("0.0.0.0:8787",
		func() (string, error) { return "gateway.local", nil },
		func() string { return "" })
	if !noRoute.Equal(config.LeaderAddress{"ws://gateway.local:8787"}) {
		t.Fatalf("with no route, discovered %v", noRoute)
	}

	neither := discoverAddresses("0.0.0.0:8787",
		func() (string, error) { return "", errors.New("no hostname") },
		func() string { return "" })
	if !neither.Empty() {
		t.Fatalf("with neither, discovered %v, want nothing rather than a port with no host", neither)
	}
}

// A hostname here is an address the node dials and fails on, and cleartext
// ws:// is only accepted for loopback.
func TestALoopbackBindOffersOnlyLoopback(t *testing.T) {
	got := discoverAddresses("127.0.0.1:8787",
		func() (string, error) { return "gateway.local", nil },
		func() string { return "192.0.2.10" })

	if !got.Equal(config.LeaderAddress{"ws://127.0.0.1:8787"}) {
		t.Fatalf("a loopback bind discovered %v", got)
	}
}

// A node refuses a bare host, so the scheme is added; the port is not, because
// the TLS terminator answers on its own.
func TestAConfiguredAddressIsTakenVerbatimExceptTheScheme(t *testing.T) {
	for name, tc := range map[string]struct {
		configured string
		want       config.LeaderAddress
	}{
		"a bare host":        {"gateway.example.com", config.LeaderAddress{"wss://gateway.example.com"}},
		"a host and a port":  {"gateway.example.com:8443", config.LeaderAddress{"wss://gateway.example.com:8443"}},
		"spelled out":        {"wss://gateway.example.com/", config.LeaderAddress{"wss://gateway.example.com/"}},
		"a name and an IP":   {"gw.example.com,192.0.2.10:8443", config.LeaderAddress{"wss://gw.example.com", "wss://192.0.2.10:8443"}},
		"spaces and commas":  {" gw.example.com ,, 192.0.2.10 ", config.LeaderAddress{"wss://gw.example.com", "wss://192.0.2.10"}},
		"the same one twice": {"gw.example.com gw.example.com", config.LeaderAddress{"wss://gw.example.com"}},
		"nothing":            {"   ", nil},
	} {
		if got := parseAdvertised(tc.configured); !got.Equal(tc.want) {
			t.Errorf("%s: parseAdvertised(%q) = %v, want %v", name, tc.configured, got, tc.want)
		}
	}
}
