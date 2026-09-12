package nodehost

import (
	"context"
	"slices"
	"strings"

	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/journal"
)

// Set late rather than in Options: the Slack gateway that answers these is
// built after the listener has started.
func (h *Host) WithOnboarding(references func() []config.AgentReference,
	unconfigured func(ctx context.Context, node Node), settled func(node Node)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.opts.References = references
	h.opts.OnUnconfiguredNode = unconfigured
	h.opts.OnNodeSettled = settled
}

func (h *Host) onboarding() (func() []config.AgentReference, func(context.Context, Node)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.opts.References, h.opts.OnUnconfiguredNode
}

func (h *Host) settle(node *attached) {
	h.mu.Lock()
	settled := h.opts.OnNodeSettled
	h.mu.Unlock()
	if settled == nil || node.userID == "" {
		return
	}
	settled(Node{NodeID: node.nodeID, UserID: node.userID, Selector: node.selector, AttachedAt: node.attachedAt})
}

func (h *Host) reviewClaim(node *attached, ad agentwire.Advertisement) {
	if ad.Empty() {
		h.reportUnconfigured(node)
		return
	}
	h.settle(node)
	h.reportUnservable(node)
}

func (h *Host) reportUnconfigured(node *attached) {
	h.log.Warn("a runtime node attached with no agent profiles; offering its owner the setup form",
		"node_id", node.nodeID, "user_id", node.userID)
	h.rec.Record(context.Background(), journal.Event{
		Stream:  journal.StreamGateway,
		Kind:    "node",
		Level:   journal.LevelWarn,
		Summary: "A runtime node attached with nothing configured",
		Keys:    journal.Keys{UserID: node.userID},
		Payload: map[string]any{
			"state":    "unconfigured",
			"node_id":  node.nodeID,
			"selector": node.selector,
		},
	})
	_, unconfigured := h.onboarding()
	if unconfigured == nil || node.userID == "" {
		return
	}
	owner := Node{NodeID: node.nodeID, UserID: node.userID, Selector: node.selector, AttachedAt: node.attachedAt}
	go unconfigured(context.Background(), owner)
}

func (h *Host) reportUnservable(node *attached) {
	references, _ := h.onboarding()
	if references == nil {
		return
	}
	refs := references()
	if len(refs) == 0 {
		return
	}
	served := h.fleetProfiles(node.userID)
	unresolved := config.UnresolvedAgents(refs, served)
	if len(unresolved) == 0 {
		return
	}
	fields := make([]string, 0, len(unresolved))
	names := make([]string, 0, len(unresolved))
	for _, ref := range unresolved {
		fields = append(fields, ref.Field+"="+ref.Name)
		if !slices.Contains(names, ref.Name) {
			names = append(names, ref.Name)
		}
	}
	h.log.Warn("this gateway names agent profiles no node in the fleet serves",
		"user_id", node.userID, "node_id", node.nodeID,
		"unresolved", strings.Join(fields, " "), "served", strings.Join(served, " "))
	h.rec.Record(context.Background(), journal.Event{
		Stream:  journal.StreamGateway,
		Kind:    "node",
		Level:   journal.LevelWarn,
		Summary: "This gateway names agent profiles the fleet does not serve",
		Keys:    journal.Keys{UserID: node.userID},
		Payload: map[string]any{
			"state":      "unservable",
			"node_id":    node.nodeID,
			"unresolved": fields,
			"agents":     names,
			"served":     served,
		},
	})
}

// Addressed by node id, not by user, because one form configures only the one
// node that asked.
func (h *Host) Configure(ctx context.Context, nodeID string, cfg agentwire.NodeConfiguration) (agentwire.NodeConfigured, error) {
	h.mu.Lock()
	var target *attached
	for _, node := range h.nodes {
		if node.nodeID == nodeID {
			if target == nil || node.attachedAt.After(target.attachedAt) {
				target = node
			}
		}
	}
	h.mu.Unlock()
	if target == nil {
		return agentwire.NodeConfigured{}, agentruntime.ErrNoNode
	}
	return target.client.Configure(ctx, cfg)
}

func (h *Host) fleetProfiles(userID string) []string {
	fleet := h.fleetFor(h.connected(), userID)
	var out []string
	h.mu.Lock()
	for _, node := range fleet {
		for _, name := range node.ad.Profiles {
			if !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	h.mu.Unlock()
	slices.Sort(out)
	return out
}
