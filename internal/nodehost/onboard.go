package nodehost

import (
	"context"
	"slices"
	"strings"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/journal"
)

// This file is what happens at the moment a node's claim first exists: the two
// questions #198 makes answerable only here.
//
// # 1. Is the gateway's configuration servable?
//
// Until this item the gateway held agent profile BODIES and could resolve
// `chat.defaults.agent` against one at write time. It no longer can — the bodies
// live on nodes — so the check moves to CONNECT time, and the source of truth
// becomes what the attaching user's fleet advertises. See
// internal/config/agentrefs.go for the reference list and internal/config/role.go
// for why Validate stopped answering it.
//
// Two properties this check must have, and both are constraints rather than
// preferences:
//
// It is ADVISORY. A gateway restarting before any node has dialled has an empty
// registry; a check that could fail would refuse the gateway's own working
// configuration on every boot, which is the "guard that fails when nothing is
// wrong" #170 warns gets disabled within a week.
//
// It is FLEET-SCOPED, not a flat union over every connected node. Delegation
// picks from the initiating user's own nodes or ones they hold a grant on and
// never a mixture (#170 Change G), so a profile only Bob's laptop serves cannot
// answer for Alice. A flat union would report a configuration as fine for a user
// it cannot serve — the more dangerous of the two wrong answers, because it is
// silent.
//
// # 2. Has this node ever been configured?
//
// A node that advertises nothing has no profiles to serve. #170 Change I makes
// that an ONBOARDING TRIGGER rather than an error: run the existing Slack form
// against the node's OWNER, who is known from the credential the connection
// presented. The node has no Slack of its own, so this can only be driven from
// here.
//
// Both are reported through the journal, never announced. That is the same rule
// disconnects follow and for the same reason: a laptop reattaches every morning,
// and a DM on every attach trains the one admin who would act on the message
// that matters to ignore it. The onboarding trigger is the one exception, and it
// is an exception because it is addressed to somebody who has not finished
// installing rather than to somebody being told about routine traffic.

// WithOnboarding supplies the two gateway-side answers this file needs.
//
// Late-bound rather than taken in Options for the reason FollowLeader is: the
// listener belongs to the binary and starts with the process, while the Slack
// gateway that answers these is built inside the daemon's run and cannot exist
// until the Slack identity has been resolved. A Host with neither set journals
// what it sees and disturbs nobody, which is exactly right for a gateway that
// opened a node port and has no Slack side at all.
func (h *Host) WithOnboarding(references func() []config.AgentReference,
	unconfigured func(ctx context.Context, node Node), settled func(node Node)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.opts.References = references
	h.opts.OnUnconfiguredNode = unconfigured
	h.opts.OnNodeSettled = settled
}

// onboarding reads the two trigger hooks under the lock. They are replaced once,
// at startup, but the replacement races every node that is already dialling.
func (h *Host) onboarding() (func() []config.AgentReference, func(context.Context, Node)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.opts.References, h.opts.OnUnconfiguredNode
}

// settle tells the gateway a node has stopped being one with nothing
// configured, so the invitation its owner is holding can be withdrawn.
//
// It fires for the two ways that becomes true and there are only two: the node
// advertised something (it was configured, by the form or by hand), or it
// disconnected. Both are known only here — the Slack side sees neither — and an
// invitation that outlives both is one an administrator cannot get out from
// behind. See gateway.WithdrawNodeSetup.
//
// Synchronous, unlike the offer next door: it posts nothing and touches no
// Slack API, so handing it a goroutine would only make the withdrawal race the
// attach that follows it.
func (h *Host) settle(node *attached) {
	h.mu.Lock()
	settled := h.opts.OnNodeSettled
	h.mu.Unlock()
	if settled == nil || node.userID == "" {
		return
	}
	settled(Node{NodeID: node.nodeID, UserID: node.userID, Selector: node.selector, AttachedAt: node.attachedAt})
}

// reviewClaim answers both questions for one connection.
//
// It runs on attach and on every later advertisement change, because a node that
// arrives unconfigured and is then configured has answered the first question
// differently — and a node whose owner edits its profiles can invalidate a
// gateway reference that resolved a minute ago.
//
// It never blocks the caller on Slack: the onboarding trigger is handed to a
// goroutine because it posts a message, and the attach path is holding the
// connection's own handler.
func (h *Host) reviewClaim(node *attached, ad agentwire.Advertisement) {
	if ad.Empty() {
		h.reportUnconfigured(node)
		return
	}
	// A node that claims something is no longer a node with nothing configured,
	// whether the form did it or its owner did it in a terminal. Say so before
	// the fleet check below, which is about the GATEWAY's references and not
	// about this node's onboarding at all.
	h.settle(node)
	h.reportUnservable(node)
}

// reportUnconfigured journals a node that claims nothing and offers its owner
// the setup form.
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
		// No owner means no one to ask. That is not a failure state: a
		// credential minted without a user is a node nobody owns, and the
		// journal line above is the whole of what can be said about it.
		return
	}
	owner := Node{NodeID: node.nodeID, UserID: node.userID, Selector: node.selector, AttachedAt: node.attachedAt}
	go unconfigured(context.Background(), owner)
}

// reportUnservable journals the gateway references this node's fleet cannot
// serve.
//
// It reports the FLEET's shortfall rather than this node's, deliberately. A node
// serving one profile out of a fleet of four is the normal shape — a link IS an
// agent — so naming what this connection alone cannot serve would fire on every
// healthy attach in a multi-node fleet and mean nothing.
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

// Configure hands one connected node the profiles an onboarding form produced,
// and reports what the node did with them.
//
// The node is addressed by id rather than by user, because a fleet's other nodes
// are already configured and one form configures the one node that asked. A node
// that has disconnected between the form opening and its submission is
// ErrNoNode: the operator is told, and the node will trigger onboarding again
// the next time it attaches, which is a better answer than writing the profiles
// somewhere they will not be read.
func (h *Host) Configure(ctx context.Context, nodeID string, cfg agentwire.NodeConfiguration) (agentwire.NodeConfigured, error) {
	h.mu.Lock()
	var target *attached
	for _, node := range h.nodes {
		if node.nodeID == nodeID {
			// Newest wins, matching the registry's own collapse: a node holding
			// two live connections through a credential rotation is one node,
			// and its newest connection is the one that will survive the old
			// credential being revoked.
			if target == nil || node.attachedAt.After(target.attachedAt) {
				target = node
			}
		}
	}
	h.mu.Unlock()
	if target == nil {
		return agentwire.NodeConfigured{}, ErrNoNode
	}
	return target.client.Configure(ctx, cfg)
}

// fleetProfiles is every profile name the fleet of one user advertises, sorted
// and deduplicated.
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
