package gateway

import (
	"context"
	"strings"
	"sync"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh/internal/onboarding"
	"github.com/miere/murtaugh/internal/slack/agentcard"
)

// Unlike AgentProfileWriter, the node may refuse because it already holds its own
// configuration, and that refusal is shown to the operator rather than swallowed.
type NodeProfileWriter func(ctx context.Context, nodeID string, profiles onboarding.Profiles) error

// Without a writer an unconfigured node is only journalled, because offering a form
// that cannot be applied is worse than saying nothing.
func (a *Gateway) WithNodeProfileWriter(write NodeProfileWriter) *Gateway {
	a.writeNodeProfiles = write
	return a
}

type pendingNodes struct {
	mu     sync.Mutex
	byUser map[string]invitation
}

type invitation struct {
	nodeID string
	carded bool
}

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

func (p *pendingNodes) withdraw(userID, nodeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if held, ok := p.byUser[userID]; ok && held.nodeID == nodeID {
		delete(p.byUser, userID)
	}
}

func (a *Gateway) OfferNodeSetup(ctx context.Context, nodeID, userID string) {
	nodeID = strings.TrimSpace(nodeID)
	userID = strings.TrimSpace(userID)
	if a.writeNodeProfiles == nil || a.agentCards == nil || nodeID == "" || userID == "" {
		return
	}
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

// Without it, an admin who once plugged in an unconfigured node is routed to that
// node forever, because the node is checked before the admin.
func (a *Gateway) WithdrawNodeSetup(nodeID, userID string) {
	nodeID = strings.TrimSpace(nodeID)
	userID = strings.TrimSpace(userID)
	if nodeID == "" || userID == "" {
		return
	}
	a.pendingNodes.withdraw(userID, nodeID)
}

type setupSubject struct {
	nodeID    string
	user      string
	configDir string
}

func (s setupSubject) isNode() bool { return s.nodeID != "" }

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

func (a *Gateway) resolveSetupDestination(ctx context.Context, subject setupSubject) (string, error) {
	if subject.isNode() {
		return a.resolveUserDM(ctx, subject.user)
	}
	return a.resolveSuggestionDestination(ctx, "")
}

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
