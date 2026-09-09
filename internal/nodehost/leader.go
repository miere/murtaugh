package nodehost

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"unicode"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/nodesocket"
)

// Only the elected gateway accepts runtime nodes.
//
// The listener is NOT what enforces that. It binds at process start and stays
// bound, because rebinding a port on every failover is a worse problem than the
// one it would solve: a standby that has to acquire a port at the moment it is
// promoted can be beaten to it by the process it is taking over from, and the
// failure arrives when there is least room for it. What is gated is the ACCEPT,
// one layer up, after the credential has been checked.
//
// The order — verify, then refuse — is load-bearing in the other direction too.
// A refusal names the leader's address, and naming it before the credential is
// checked would hand the location of the workspace's serving gateway to anyone
// who can reach this port. After verification it tells a node something it is
// entitled to know, and the undifferentiated 401 an unknown credential gets
// stays exactly as uninformative as it was.

// Leadership is the two questions the accept edge asks the election.
//
// It is an interface here rather than an import of internal/election because
// the answer has to arrive AFTER the Host is built: the listener starts with the
// process and the election is wired inside the daemon's run. See FollowLeader.
type Leadership interface {
	// Allow reports whether this gateway may act as the leader right now.
	//
	// It must be the election's verifying check, not its cached boolean.
	// Accepting a node is externally visible and long-lived — a suspended
	// standby that woke up still believing it leads would take a node's
	// conversations and answer none of them.
	Allow(ctx context.Context) bool

	// Leader reports where the current leader accepts runtime nodes, and
	// whether there is a leader at all. An empty address with ok=true is a
	// leader that accepts none.
	Leader(ctx context.Context) (config.LeaderAddress, bool)
}

// FollowLeader installs the election this Host defers to.
//
// Until it is called the Host accepts nothing, and that default is deliberate.
// The two ways to get this wrong are not symmetric: a Host that refuses because
// nobody wired an election can be made to SAY so, while a Host that accepts
// because nobody wired one is a standby quietly holding a node that believes
// all is well — the exact state #170 says must not exist, and the one that
// produces the support ticket with nothing in it.
//
// Saying so is this gateway's own job, not the node's. The node is answered
// with the same 503 as "nobody is elected yet", because there is nothing
// different for it to DO about the two, and it logs them with the same line —
// which reads as the benign, self-healing state. So the un-wired case is logged
// here at ERROR instead: see refuseNotLeading.
func (h *Host) FollowLeader(l Leadership) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.leadership = l
}

// leading reports whether this gateway may accept a node right now.
func (h *Host) leading(ctx context.Context) (Leadership, bool) {
	h.mu.Lock()
	l := h.leadership
	h.mu.Unlock()
	if l == nil {
		return nil, false
	}
	return l, l.Allow(ctx)
}

// refuseNotLeading turns a node away from a gateway that is not serving, saying
// where to go when it can.
//
// Three answers, because a node has three different things to do about them:
//
//   - 421 with a leader address: hop, once, without waiting out a backoff.
//   - 421 with none: the leader is up and accepts no nodes. Hopping is pointless
//     and so is retrying quickly; nobody in the fleet can serve this node.
//   - 503: no leader is elected at all. Keep every address known, keep backing
//     off, and expect one of them to answer when the election settles.
//
// A fourth state exists and is NOT one of the three: no election was wired at
// all. It is answered with the same 503, because "wait" is still the only thing
// the node can do — but it is the one state here that never heals, so it is the
// one that is logged at ERROR on this side. The node's own line for a 503 says
// "no gateway may be elected yet", which is the benign reading, and an operator
// chasing a listener that accepts nothing would follow it away from the fault.
func (h *Host) refuseNotLeading(w http.ResponseWriter, r *http.Request, l Leadership, nodeID string) {
	if l == nil {
		h.log.Error("this gateway has a node listener but no leader election wired, so it will accept no runtime node for the life of this process; nothing about this state recovers on its own",
			"node_id", nodeID)
		http.Error(w, "this gateway is not accepting runtime nodes", http.StatusServiceUnavailable)
		return
	}
	addr, ok := l.Leader(r.Context())
	if !ok {
		h.log.Info("turned a runtime node away: no gateway is currently elected", "node_id", nodeID)
		http.Error(w, "no gateway is currently elected", http.StatusServiceUnavailable)
		return
	}
	// The body is for a human reading a log; the header is what the node acts
	// on. Both are short, because the client truncates a refusal body long
	// before a helpful paragraph would fit in it.
	if addr.Empty() {
		h.log.Info("turned a runtime node away: the elected gateway accepts no runtime nodes", "node_id", nodeID)
		http.Error(w, "this gateway is not the elected one, and the elected gateway accepts no runtime nodes", nodesocket.StatusWrongGateway)
		return
	}
	w.Header().Set(nodesocket.HeaderLeader, addr.Encode())
	h.log.Info("redirected a runtime node to the elected gateway",
		"node_id", nodeID, "leader", addr.Encode())
	http.Error(w, "this gateway is not the elected one; the elected gateway is at "+addr.Primary(), nodesocket.StatusWrongGateway)
}

// DetachAll drops every attached node, so they redial and land on whoever is
// leading now.
//
// It is what demotion calls. Without it a node stays attached to a gateway that
// has stood down, holding a connection that will never carry a conversation
// again — and it cannot find out on its own, because a node has no way to ask
// whether the gateway it is attached to still leads.
func (h *Host) DetachAll(reason string) {
	nodes := h.takeAll()
	if len(nodes) == 0 {
		return
	}
	h.log.Info("dropping attached runtime nodes", "reason", reason, "nodes", len(nodes))
	for _, node := range nodes {
		node.close()
	}
}

// Address reports where runtime nodes reach this gateway, for the election to
// write into the lock record.
//
// It is nil until the listener has bound, and nil after it stops. That is a
// legitimate answer with a precise meaning — this gateway accepts no nodes,
// which is what every gateway shipping today is and what one started without
// -node-listen remains — and it is gated on the LISTENER rather than on the
// configuration for a reason. Two gateways on one machine both try to bind the
// node port and one of them loses; when that one is promoted it accepts nothing,
// and a configured address would have every standby send the whole fleet to a
// door it cannot open.
//
// # Why both a name and an address
//
// #170 requires hostname AND IP, and the reason is that neither survives alone:
// an IP moves under DHCP, a changed network or a VPN, and a hostname does not
// resolve from every network a laptop wakes up on. The hostname goes first
// because it is the durable one; the IP is the fallback for the node that cannot
// resolve it.
//
// # Why the discovered form is ws:// and what an operator does about it
//
// This process terminates no TLS, so an auto-discovered address describes what
// it actually serves rather than what one would like it to. A node handed a
// ws:// address for a non-loopback host refuses to dial it — its credential
// travels in a header — and says so in a sentence naming wss://. That refusal is
// the intended outcome: the deployment #170 sanctions puts a reverse proxy
// holding a certificate in front, and its address is not discoverable from here
// because it is a different host and a different port. Configure it, and it
// REPLACES the discovered list: that is what "a configured hostname overrides
// the auto-discovered value" means. Configure BOTH forms of it — the name and
// the address — and the pair #170 asks for survives into the deployment that can
// actually use it, which the discovered one cannot.
func (h *Host) Address() config.LeaderAddress {
	listen := h.listenAddr()
	if listen == "" {
		return nil
	}
	if advertised := parseAdvertised(h.opts.Advertise); !advertised.Empty() {
		return advertised
	}
	return discoverAddresses(listen, os.Hostname, localIP)
}

// listenAddr is the address the listener actually bound, which is not always the
// one it was asked for: a port of 0 is chosen by the kernel, and publishing the
// requested one would name a port nothing is listening on.
func (h *Host) listenAddr() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.listen
}

// setListenAddr records the bound address, once serving starts.
func (h *Host) setListenAddr(addr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.listen = addr
}

// discoverAddresses builds the candidate list from what the machine can say
// about itself.
//
// A loopback bind yields exactly one address and no names: nothing outside this
// machine can reach it, so a hostname would be an address a node dials and fails
// on. That is also the `--role both` deployment, where the single loopback entry
// is not a degraded answer but the whole correct one.
func discoverAddresses(listen string, hostname func() (string, error), ip func() string) config.LeaderAddress {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return nil
	}
	if isLoopbackHost(host) {
		return config.LeaderAddress{"ws://" + net.JoinHostPort(host, port)}
	}

	var addresses config.LeaderAddress
	if name, err := hostname(); err == nil && strings.TrimSpace(name) != "" {
		addresses = append(addresses, "ws://"+net.JoinHostPort(strings.TrimSpace(name), port))
	}
	// A bind to one specific address is the truth about where this gateway can
	// be reached; the routing-table lookup is only consulted when the bind is
	// unspecified and cannot answer.
	local := host
	if unspecifiedHost(host) {
		local = ip()
	}
	if strings.TrimSpace(local) != "" {
		addresses = append(addresses, "ws://"+net.JoinHostPort(local, port))
	}
	return config.ParseLeaderAddress(addresses.Encode())
}

// parseAdvertised reads what an operator configured: one address, or several
// separated by spaces or commas, so the name-and-address pair #170 asks for can
// be offered by the deployment that can actually use it.
//
// Each is taken EXACTLY as written apart from a missing scheme, which becomes
// wss:// — a bare host names a machine on a network, and a node will not put its
// credential on that in cleartext.
//
// Nothing else is inferred, and the port least of all. The deployment this
// option exists for puts a TLS terminator in front, and its port is its own —
// 443, usually — not the one this process happens to have bound behind it.
// Completing a bare hostname with the listener's port would silently point the
// whole fleet at a port nothing answers on, which is a worse failure than an
// address an operator can read back and see is wrong.
func parseAdvertised(configured string) config.LeaderAddress {
	fields := strings.FieldsFunc(configured, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
	addresses := make(config.LeaderAddress, 0, len(fields))
	for _, field := range fields {
		if !strings.Contains(field, "://") {
			field = "wss://" + field
		}
		addresses = append(addresses, field)
	}
	return config.ParseLeaderAddress(addresses.Encode())
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	parsed := net.ParseIP(host)
	return parsed != nil && parsed.IsLoopback()
}

// unspecifiedHost reports a bind that names no interface: "", 0.0.0.0 or ::.
func unspecifiedHost(host string) bool {
	if strings.TrimSpace(host) == "" {
		return true
	}
	parsed := net.ParseIP(host)
	return parsed != nil && parsed.IsUnspecified()
}

// localIP returns the address of the interface that would carry outbound
// traffic.
//
// The UDP "dial" sends nothing — connecting a datagram socket only makes the
// kernel choose a route and bind a local address — so this is a routing-table
// lookup rather than a network call, and it works offline. It beats walking
// net.Interfaces(), which cannot tell which of several addresses is the one that
// matters.
func localIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	return addr.IP.String()
}
