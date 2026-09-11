package nodehost

import (
	"slices"
	"sync"
	"time"

	"github.com/miere/murtaugh/internal/agent/remote"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/agentwire"
)

// This file is the registry: which nodes are connected, and what each one says
// it can serve. It replaces the single slot #193 shipped, where a second node
// evicted the first.
//
// # It is in memory, and that is a decision rather than an omission
//
// #170's table puts "node token hashes, node registry, conversation pins" in
// the gateway's column, which is a statement about who OWNS them and not about
// where they are written down. A registry entry is a live socket plus the claim
// that arrived over it; both die with the process, so persisting one would
// store a fact that stops being true at the moment of the crash that made
// anybody read it back. Two things settle it. Only the elected leader accepts
// node connections (#170 Change H), so a persisted registry read by a standby
// is guaranteed stale. And the half of this that genuinely outlives a process —
// the conversation PIN — is the half #170 says to store, which item 10 does, in
// the side-store family internal/config/nodetokens.go established.
//
// # A connection is the unit, a node is what you ask about
//
// The map is keyed per CONNECTION, not per node, because #170 requires two
// credentials to be valid at once so a token can be rotated with no downtime —
// and CloseCredential closes by selector precisely so revoking the old one does
// not drop the node. A map keyed by node id would make the rotation's second
// connection evict the first and undo that.
//
// Enumeration goes the other way: Nodes collapses to one entry per node id,
// because delegation must never see one node twice and round-robin it against
// itself. The two views are not in tension — one is about lifetime, the other
// about identity.
//
// # A conversation is bound to a connection, and that is not delegation
//
// Choosing which node a new conversation opens on needs a conversation key, a
// fleet and a stored pin, and is item 10. Keeping a conversation on the node it
// opened on needs none of those and could not wait for them: session ids are
// minted per node, so once there is more than one connection, "send this
// session's next turn to whichever node is newest" sends it to a machine that
// never issued it. See Host.sessions.

// Node is one connected node as the gateway sees it: who it is, when it
// arrived, and what it claims.
//
// It is a value, copied out of the registry under the lock, because the only
// safe thing to hand a caller outside the mutex is a copy: a pointer into the
// map is one a disconnect is about to mutate.
type Node struct {
	// NodeID and UserID come from the CREDENTIAL the connection presented,
	// never from anything the node said. #170's rule is that a node must never
	// assert its own identity: inside a fleet, a node that announces who it is
	// can announce somebody else. That is why there is no node id anywhere in
	// agentwire.Advertisement.
	NodeID string
	UserID string
	// Selector names the credential, and is what revocation closes by.
	Selector string
	// AttachedAt is when the handshake completed. It is the tie-break Nodes
	// applies when one node holds two live connections through a rotation.
	AttachedAt time.Time
	// Advertisement is the node's latest full claim. The zero value means the
	// node has claimed nothing — an unconfigured node, which #170 makes an
	// onboarding trigger rather than an error.
	Advertisement agentwire.Advertisement
}

// attached is one live node connection: the registry's internal entry.
type attached struct {
	// connID keys the registry. It is a local counter and never crosses the
	// wire: it names a socket, not a machine.
	connID     string
	client     *remote.Client
	selector   string
	nodeID     string
	userID     string
	attachedAt time.Time
	closed     chan struct{}
	// closeOnce guards the close of `closed`. Four goroutines reach close()
	// for one entry — the link's own serve loop, shutdown's detachAll,
	// revocation's CloseCredential, and a redial displacing this connection —
	// and any two of them can observe the channel open at the same instant. A
	// check-then-close there is `close of closed channel`, which is a panic no
	// caller recovers: it takes the whole gateway daemon down, on shutdown or
	// on a revocation, which are the two moments nobody is watching.
	closeOnce sync.Once

	// ad is guarded by the Host's mutex, not by one of its own. Everything
	// per-node lives behind that single lock, and nothing under it calls out,
	// so there is no ordering question to get wrong. Which of two claims is the
	// newer one is settled a layer down, in remote.Client, where the handshake
	// answer and the pushed change converge.
	ad          agentwire.Advertisement
	credentials map[string]agentruntime.CredentialHealth
}

// snapshot copies the entry out. Callers must hold the Host's mutex.
func (n *attached) snapshot() Node {
	return Node{
		NodeID:        n.nodeID,
		UserID:        n.userID,
		Selector:      n.selector,
		AttachedAt:    n.attachedAt,
		Advertisement: n.ad.Clone(),
	}
}

// Nodes is every connected node and what it claims, one entry per NODE.
//
// A node holding two live connections through a credential rotation appears
// once, as its most recent connection: a list that named the same machine twice
// would round-robin a node against itself and call the result balance. The
// order is stable — by node id — so a caller that walks it makes the same
// choice on two gateways given the same fleet.
//
// It is the registry's read surface, and today its readers are this package's
// tests and Attached; the caller it is shaped for is delegation, which is item
// 10 and which reuses this collapse rather than writing a second one.
func (h *Host) Nodes() []Node {
	h.mu.Lock()
	defer h.mu.Unlock()
	nodes := h.collapsedLocked()
	out := make([]Node, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, node.snapshot())
	}
	return out
}

// collapsedLocked is the registry as one entry per NODE, ordered by node id.
// Callers hold the mutex.
//
// It is the one implementation of that collapse, and delegation uses it rather
// than deriving its own: a second copy of "which connection represents this
// machine, and in what order" would be a second rotation tie-break, and only
// one of them would be on the turn path. Nodes copies values out of it for
// callers outside the lock; delegation keeps the pointers, because it needs the
// clients.
func (h *Host) collapsedLocked() []*attached {
	newest := make(map[string]*attached, len(h.nodes))
	for _, node := range h.nodes {
		if held, ok := newest[node.nodeID]; ok && !node.attachedAt.After(held.attachedAt) {
			continue
		}
		newest[node.nodeID] = node
	}
	out := make([]*attached, 0, len(newest))
	for _, node := range newest {
		out = append(out, node)
	}
	slices.SortFunc(out, func(a, b *attached) int {
		switch {
		case a.nodeID < b.nodeID:
			return -1
		case a.nodeID > b.nodeID:
			return 1
		default:
			return 0
		}
	})
	return out
}

// Attached reports the node the gateway currently addresses, if any.
//
// It answers for the same connection anyClient() resolves to, which is what
// makes it a usable readiness check: a caller that sees a node here can send to
// it. It is NOT the node any particular conversation runs on — that is
// delegation's answer and it is per conversation. With several attached this is
// the most recent.
func (h *Host) Attached() (nodeID string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	node := h.newest()
	if node == nil {
		return "", false
	}
	return node.nodeID, true
}

// insert publishes a connection and returns any connection it displaced.
//
// Replacement is keyed by SELECTOR, not by node id, and the difference is the
// rotation. Two credentials are valid at once by design so a token can be
// replaced with no downtime window; a node that dialled back in on its new
// credential while the old connection is still up is one node with two
// connections, and evicting the old one here would undo the overlap that exists
// to make the operation seamless. A node redialling after a network drop
// presents the SAME credential, which is the case this does displace — the
// gateway is very often still holding a half-dead socket it has not noticed.
func (h *Host) insert(node *attached) *attached {
	h.mu.Lock()
	defer h.mu.Unlock()
	var displaced *attached
	for id, existing := range h.nodes {
		if existing.selector == node.selector {
			displaced = existing
			delete(h.nodes, id)
			break
		}
	}
	h.nodes[node.connID] = node
	return displaced
}

// remove drops one connection from the registry.
func (h *Host) remove(node *attached) {
	h.mu.Lock()
	if h.nodes[node.connID] == node {
		delete(h.nodes, node.connID)
	}
	h.pruneSessionsLocked(node)
	node.credentials = nil
	h.mu.Unlock()
}

// takeAll empties the registry and returns what was in it. Shutdown's half of
// detachAll.
//
// It returns a slice rather than closing under the lock because close() reaches
// the link, and holding the registry's mutex across a network teardown would
// stall every accept and every delegation behind it.
func (h *Host) takeAll() []*attached {
	h.mu.Lock()
	out := make([]*attached, 0, len(h.nodes))
	for id, node := range h.nodes {
		out = append(out, node)
		delete(h.nodes, id)
		h.pruneSessionsLocked(node)
	}
	h.mu.Unlock()
	return out
}

// takeCredential removes every connection authenticated with one credential.
//
// It iterates, and the honest reason is narrower than it looks: today it can
// never find more than one, because insert deletes any existing entry sharing a
// selector before publishing the newcomer, under this same mutex. So "closes
// every connection on the credential" is enforced by insert's invariant, and
// this loop is what keeps the two agreeing if that invariant is ever relaxed —
// a rotation that let both connections live, say. It is not the thing standing
// between a revoked credential and a serving node.
//
// The revocation hole that IS reachable is a different one and is not closed
// here: a connection whose handshake is in flight when its credential is
// revoked is verified before it is published, so it attaches afterwards and
// nothing closes it. That ordering is unchanged since item 7 and closing it
// means re-checking the credential at publication; it is written down because
// the comment that used to sit here pointed at a property that cannot be
// violated while this one can.
func (h *Host) takeCredential(selector string) []*attached {
	h.mu.Lock()
	var out []*attached
	for id, node := range h.nodes {
		if node.selector == selector {
			out = append(out, node)
			delete(h.nodes, id)
			h.pruneSessionsLocked(node)
		}
	}
	h.mu.Unlock()
	return out
}

// newest is the most recently attached connection, or nil. Callers hold the
// mutex.
func (h *Host) newest() *attached {
	var pick *attached
	for _, node := range h.nodes {
		if pick == nil || node.attachedAt.After(pick.attachedAt) {
			pick = node
		}
	}
	return pick
}

// setAdvertisement records what a node claims and reports whether that node is
// in the registry yet.
//
// It is called before publication as well as after: the opening claim arrives
// from inside the handshake, which is on purpose — the entry must be complete
// at the moment it is published, not a beat later. The return value is what
// keeps that out of the journal, because an opening claim is part of the attach
// event and a second line saying the same thing is the noise that makes a
// journal unreadable.
func (h *Host) setAdvertisement(node *attached, ad agentwire.Advertisement) (published bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	node.ad = ad
	return h.nodes[node.connID] == node
}
