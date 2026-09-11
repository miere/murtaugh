package nodehost

import (
	"slices"
	"sync"
	"time"

	"github.com/miere/murtaugh/internal/agent/remote"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/agentwire"
)

// A value, not a pointer: a pointer into the registry could be changed by a
// disconnect while the caller is still reading it.
type Node struct {
	// From the credential, never from the node: a node that could name itself
	// could name another.
	NodeID        string
	UserID        string
	Selector      string
	AttachedAt    time.Time
	Advertisement agentwire.Advertisement
}

type attached struct {
	connID     string
	client     *remote.Client
	selector   string
	nodeID     string
	userID     string
	attachedAt time.Time
	closed     chan struct{}
	closeOnce  sync.Once

	ad          agentwire.Advertisement
	credentials map[string]agentruntime.CredentialHealth
}

func (n *attached) snapshot() Node {
	return Node{
		NodeID:        n.nodeID,
		UserID:        n.userID,
		Selector:      n.selector,
		AttachedAt:    n.attachedAt,
		Advertisement: n.ad.Clone(),
	}
}

// One entry per node, so a rotating node is not round-robinned against itself;
// sorted by id so two gateways given the same fleet choose alike.
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

// Not the node any particular conversation runs on; that is decided per
// conversation by delegation.
func (h *Host) Attached() (nodeID string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	node := h.newest()
	if node == nil {
		return "", false
	}
	return node.nodeID, true
}

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

func (h *Host) remove(node *attached) {
	h.mu.Lock()
	if h.nodes[node.connID] == node {
		delete(h.nodes, node.connID)
	}
	h.pruneSessionsLocked(node)
	node.credentials = nil
	h.mu.Unlock()
}

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

func (h *Host) newest() *attached {
	var pick *attached
	for _, node := range h.nodes {
		if pick == nil || node.attachedAt.After(pick.attachedAt) {
			pick = node
		}
	}
	return pick
}

func (h *Host) setAdvertisement(node *attached, ad agentwire.Advertisement) (published bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	node.ad = ad
	return h.nodes[node.connID] == node
}
