package gateway

import (
	"context"
	"strings"
	"sync"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh/internal/onboarding"
	"github.com/miere/murtaugh/internal/slack/agentcard"
)

// This file is #170 Change I's onboarding TRIGGER, and the word matters: it is a
// trigger, not a second flow.
//
// A runtime node that attaches advertising no agent profiles has never been
// configured. The existing setup form — the one a fresh gateway offers its
// administrator — is exactly the right conversation to have, so this offers that
// form, to the node's OWNER rather than to the administrator, and sends the
// answers to the node instead of writing them here. Nothing about the form, its
// steps, its model discovery or the profiles it produces changes.
//
// # Why the gateway drives it
//
// A node has no Slack connection. It cannot post a card, open a modal, or read a
// submission. The gateway holds the workspace's tokens and it knows who owns the
// node, because the node's identity came from the credential its connection
// presented rather than from anything the node said. So this is the only side
// that can ask, and internal/nodehost is where the question first becomes
// answerable.
//
// # Why a non-administrator may open the form
//
// Every other path into this form is gated on being the gateway administrator,
// deliberately: it writes agent profiles into the GATEWAY's own store, and an
// agent there answers everybody. This one writes into the asking user's own
// node, which they already own and can already reconfigure with a terminal. The
// gate is therefore not relaxed but replaced: a user may open the form when they
// have an unconfigured node waiting, and the invitation ends the moment it stops
// being true — they used it, the node was configured some other way, or the node
// disconnected. Nobody can reach this by clicking a card that was not addressed
// to them, because the pending set is keyed by user and populated only by an
// attach.

// NodeProfileWriter applies a completed form to one runtime node.
//
// Like AgentProfileWriter it is a closure supplied by the composition root, so
// this package stays free of the node registry and the wire. Unlike it, the node
// may REFUSE — a node applies configuration only while it holds none of its own
// — and that refusal is reported to the operator as the outcome rather than
// swallowed.
type NodeProfileWriter func(ctx context.Context, nodeID string, profiles onboarding.Profiles) error

// WithNodeProfileWriter enables node onboarding. Without one an unconfigured
// node is journalled and its owner is not disturbed: offering a form that cannot
// be applied is worse than saying nothing.
func (a *Gateway) WithNodeProfileWriter(write NodeProfileWriter) *Gateway {
	a.writeNodeProfiles = write
	return a
}

// pendingNodes is the set of users who have an unconfigured node waiting.
//
// In memory and per gateway, because the fact it records is: the node is
// attached to THIS process, and a gateway that restarts loses the connection
// along with the entry. The node re-triggers on its next attach, which happens
// within seconds.
type pendingNodes struct {
	mu sync.Mutex
	// byUser maps a Slack user id to that user's outstanding invitation. One
	// entry per user rather than a list: a user with two unconfigured nodes is
	// offered the form once, configures one, and is offered it again when the
	// other re-advertises — which is better than two identical cards in a DM
	// that differ only by an id the operator cannot see.
	byUser map[string]invitation
}

// invitation is one user's outstanding entitlement to the form.
//
// The two fields answer different questions and must not be collapsed into one.
// nodeID is WHICH node a submission configures, and it moves — the most recent
// unconfigured node of theirs to attach is the one whose card is in front of
// them. carded is WHETHER they have already been sent that card, and it does not
// move: it is what makes the card once per owner.
//
// Keying "already offered" on the node id instead is the DM storm this exists to
// stop. Two unconfigured nodes on a seconds-scale reconnect backoff — a laptop
// and a desktop, both freshly installed — alternate, so every attach finds a
// different id stored and posts again.
type invitation struct {
	nodeID string
	carded bool
}

// offer records an invitation and reports whether a card should be posted.
//
// A node that has nothing configured attaches, is offered the form, and — if its
// owner is not at their desk — keeps attaching: a laptop lid, a flaky network, a
// restart. Posting on every attach is the "trains the admin to ignore it"
// failure #170 states for disconnects, arriving in the one conversation that
// most needs to be read.
func (p *pendingNodes) offer(userID, nodeID string) (fresh bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.byUser == nil {
		p.byUser = make(map[string]invitation, 1)
	}
	already := p.byUser[userID].carded
	p.byUser[userID] = invitation{nodeID: nodeID, carded: true}
	return !already
}

func (p *pendingNodes) get(userID string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	held, ok := p.byUser[userID]
	return held.nodeID, ok
}

func (p *pendingNodes) clear(userID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.byUser, userID)
}

// withdraw drops an invitation that names this node, and only one that does.
//
// The node-id check is the whole of it: a user with a second unconfigured node
// waiting has that node's id stored, and withdrawing on the first node's news
// would cancel an entitlement that is still live.
func (p *pendingNodes) withdraw(userID, nodeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if held, ok := p.byUser[userID]; ok && held.nodeID == nodeID {
		delete(p.byUser, userID)
	}
}

// OfferNodeSetup tells a node's owner that their node has nothing configured,
// and hands them the form.
//
// Best-effort and silent when there is nobody to tell, exactly as NotifyNoAgents
// is: a node whose credential names no user, or a gateway with no messaging
// surface, produces a journal line and no message. It is called from the node
// registry's attach path, off the connection's own goroutine.
func (a *Gateway) OfferNodeSetup(ctx context.Context, nodeID, userID string) {
	nodeID = strings.TrimSpace(nodeID)
	userID = strings.TrimSpace(userID)
	if a.writeNodeProfiles == nil || a.agentCards == nil || nodeID == "" || userID == "" {
		return
	}
	// Recorded BEFORE the card is posted. The card's button carries no payload —
	// it is the same button the administrator's prompt uses — so the click is
	// matched back to a node by who clicked it, and an invitation recorded after
	// the post would leave a window in which a fast operator's click was refused.
	//
	// The entitlement is refreshed on every attach; only the CARD is once. A node
	// whose owner has not got round to it reattaches all day, and the card
	// already sitting in their DM still opens the form.
	if !a.pendingNodes.offer(userID, nodeID) {
		return
	}

	dest, err := a.resolveUserDM(ctx, userID)
	if err != nil || dest == "" {
		a.logger.Warn("cannot offer node setup: no conversation with the node's owner",
			"error", err, "user", userID, "node_id", nodeID)
		return
	}
	blocks, err := a.agentCards.Prompt()
	if err != nil {
		a.logger.Warn("could not render the node setup prompt", "error", err)
		return
	}
	if _, err := a.postRawCard(ctx, dest, blocks, agentcard.PlainText()); err != nil {
		a.logger.Warn("could not post the node setup prompt", "error", err, "user", userID)
		return
	}
	a.logger.Info("offered the setup form to the owner of an unconfigured runtime node",
		"user", userID, "node_id", nodeID)
}

// WithdrawNodeSetup ends an invitation, because the node it named stopped being
// a node with nothing configured — it was configured, or it went away.
//
// Without it an invitation outlives its node for the life of the process, and
// the cost is not the stale entry but where setupSubjectFor looks first: the
// node branch is checked BEFORE the admin branch, so an administrator who once
// plugged in an unconfigured node is routed to that node id forever, and their
// own gateway's setup form becomes unreachable. Configuring a node by hand, or
// unplugging it, has to be able to say so.
//
// Silent when the invitation names a different node: see pendingNodes.withdraw.
func (a *Gateway) WithdrawNodeSetup(nodeID, userID string) {
	nodeID = strings.TrimSpace(nodeID)
	userID = strings.TrimSpace(userID)
	if nodeID == "" || userID == "" {
		return
	}
	a.pendingNodes.withdraw(userID, nodeID)
}

// setupSubject is who a submission of the setup form is configuring.
//
// The two cases differ in three ways and in nothing else: which store the
// profiles land in, which user the `tweaker` profile is bound to, and which
// directory that profile is rooted in. Resolving all three in one place is what
// keeps the form itself unaware that there are two cases at all.
type setupSubject struct {
	// nodeID is the node being configured, empty for the gateway itself.
	nodeID string
	// user is the Slack ID the tweaker profile is bound to and the outcome is
	// reported to.
	user string
	// configDir roots the tweaker profile. Empty for a node: the directory is on
	// the node's machine and only the node knows it, so it fills it in.
	configDir string
}

// isNode reports a submission that configures a runtime node.
func (s setupSubject) isNode() bool { return s.nodeID != "" }

// setupSubjectFor decides who a click or a submission from userID is for, and
// whether it is allowed at all.
//
// The administrator configures the gateway. Anybody with an unconfigured node
// waiting configures that node. Anybody else is refused — which is the same
// refusal as before this item for every user who has not just plugged a node in.
func (a *Gateway) setupSubjectFor(userID string) (setupSubject, bool) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return setupSubject{}, false
	}
	if nodeID, ok := a.pendingNodes.get(userID); ok && a.writeNodeProfiles != nil {
		return setupSubject{nodeID: nodeID, user: userID}, true
	}
	if a.access().IsAdminUser(userID) {
		return setupSubject{user: strings.TrimSpace(a.access().AdminUser), configDir: a.configDir}, true
	}
	return setupSubject{}, false
}

// resolveSetupDestination is where a completed form's outcome is reported: the
// admin's DM for the gateway, the node owner's for a node.
func (a *Gateway) resolveSetupDestination(ctx context.Context, subject setupSubject) (string, error) {
	if subject.isNode() {
		return a.resolveUserDM(ctx, subject.user)
	}
	return a.resolveSuggestionDestination(ctx, "")
}

// resolveUserDM opens a DM with one user.
//
// resolveSuggestionDestination answers the same question for the ADMIN and only
// for the admin; a node's owner is by construction somebody else. Both degrade
// the same way — no messaging surface means no destination, not a panic — on a
// path every caller treats as best-effort.
func (a *Gateway) resolveUserDM(ctx context.Context, userID string) (string, error) {
	if strings.TrimSpace(userID) == "" || a.messaging == nil {
		return "", nil
	}
	convo, _, _, err := a.messaging.OpenConversationContext(ctx,
		&slack.OpenConversationParameters{Users: []string{userID}, ReturnIM: true})
	if err != nil {
		return "", err
	}
	if convo == nil || convo.ID == "" {
		return "", nil
	}
	return convo.ID, nil
}
