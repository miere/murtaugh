package nodehost

import (
	"errors"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

// What a gateway can work out about itself, and what it must not invent.
//
// #170 asks for a hostname AND an IP because neither survives alone: an IP moves
// under DHCP, a changed network or a VPN, and a hostname does not resolve from
// every network a laptop wakes up on. The pair is assembled here, from two
// sources that each fail in their own way on a real machine, so both halves are
// injected rather than trusted.

func TestBothANameAndAnAddressAreOffered(t *testing.T) {
	got := discoverAddresses("0.0.0.0:8787",
		func() (string, error) { return "gateway.local", nil },
		func() string { return "192.0.2.10" })

	want := config.LeaderAddress{"ws://gateway.local:8787", "ws://192.0.2.10:8787"}
	if !got.Equal(want) {
		t.Fatalf("discovered %v, want the name first and the address behind it: %v", got, want)
	}
}

// A bind to one interface has already answered the question the routing table
// would be asked. Consulting it anyway would publish the address of whichever
// interface happens to reach the internet, which is not where this gateway is
// listening.
func TestASpecificBindIsPreferredToTheRoutingTable(t *testing.T) {
	got := discoverAddresses("192.0.2.7:8787",
		func() (string, error) { return "gateway.local", nil },
		func() string { return "198.51.100.1" })

	want := config.LeaderAddress{"ws://gateway.local:8787", "ws://192.0.2.7:8787"}
	if !got.Equal(want) {
		t.Fatalf("discovered %v, want the bound address rather than the routed one: %v", got, want)
	}
}

// Either half may be missing on a real machine — a host with no name, a machine
// with no route — and one address is worth publishing. None is not.
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

// A loopback bind is the `--role both` deployment, and its one address is the
// whole correct answer rather than a degraded one: a hostname there is an
// address a node dials and fails on, and it is also the only form the node's own
// transport rule will accept in cleartext.
func TestALoopbackBindOffersOnlyLoopback(t *testing.T) {
	got := discoverAddresses("127.0.0.1:8787",
		func() (string, error) { return "gateway.local", nil },
		func() string { return "192.0.2.10" })

	if !got.Equal(config.LeaderAddress{"ws://127.0.0.1:8787"}) {
		t.Fatalf("a loopback bind discovered %v", got)
	}
}

// A configured address is used as written. The one thing that is added is the
// scheme, because a bare host on a real network is exactly what a node refuses
// to put its credential on; the one thing that must NOT be added is the port,
// because the terminator this option exists for answers on its own.
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
