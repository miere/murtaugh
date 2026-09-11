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

// An interface rather than an import of internal/election, because the
// election is wired only after the Host is built.
type Leadership interface {
	// Must be the election's verifying check, not its cached flag: a standby that
	// wakes from suspend still believing it leads must not take nodes.
	Allow(ctx context.Context) bool

	Leader(ctx context.Context) (config.LeaderAddress, bool)
}

// Until called the Host accepts nothing, on purpose: accepting unwired would
// let a standby hold nodes whose conversations go to the real leader.
func (h *Host) FollowLeader(l Leadership) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.leadership = l
}

func (h *Host) leading(ctx context.Context) (Leadership, bool) {
	h.mu.Lock()
	l := h.leadership
	h.mu.Unlock()
	if l == nil {
		return nil, false
	}
	return l, l.Allow(ctx)
}

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

// A node cannot tell that its gateway stopped leading, so demotion must drop
// it to make it redial the leader.
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

// Nil until the listener binds, not merely when configured: two gateways on
// one machine race for the port, and the loser must not advertise it.
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

func (h *Host) listenAddr() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.listen
}

func (h *Host) setListenAddr(addr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.listen = addr
}

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
	local := host
	if unspecifiedHost(host) {
		local = ip()
	}
	if strings.TrimSpace(local) != "" {
		addresses = append(addresses, "ws://"+net.JoinHostPort(local, port))
	}
	return config.ParseLeaderAddress(addresses.Encode())
}

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

func unspecifiedHost(host string) bool {
	if strings.TrimSpace(host) == "" {
		return true
	}
	parsed := net.ParseIP(host)
	return parsed != nil && parsed.IsUnspecified()
}

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
