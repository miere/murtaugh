package nodehost

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/config"
)

// cmd/murtaugh-gateway links this, so it must contribute nothing that can run
// a model; CI checks its dependency closure for that.
func Runtime(host *Host) func(config.Config, *slog.Logger) agentruntime.Builder {
	return func(cfg config.Config, logger *slog.Logger) agentruntime.Builder {
		if logger == nil {
			logger = slog.Default()
		}
		return func(hooks agentruntime.Hooks) agentruntime.Runtime {
			host.SetApprover(approverFor(cfg, hooks))
			host.setAccess(cfg.Access)
			host.setBackground(hooks.BackgroundEvents)
			host.setSignIns(hooks.SignIn)
			host.setCredentialHealth(hooks.CredentialHealth)

			rt := agentruntime.Runtime{
				CredentialReports: host.credentialReports,
				PinnedNode:        host.pinnedNode,
				ConnectedNodes:    host.connectedNodes,
				RenewCredential:   host.renewCredential,
				Delegator: &headlessDelegator{
					host:        host,
					idleTimeout: cfg.Defaults.EffectiveRequestTimeout(),
					log:         logger.With("runtime", "node", "delegate", "headless"),
				},
			}
			if !hooks.Chat {
				return rt
			}
			rt.Sessions = make(map[string]*agent.SessionManager)
			for _, name := range agentNames(cfg) {
				profile := cfg.Agents[name]
				manager := agent.NewSessionManager(
					&nodeClient{host: host},
					cfg.Defaults.EffectiveSessionIdleTimeout(),
					cfg.Defaults.EffectiveMaxSessions(),
				).WithLogger(logger.With("agent", name, "runtime", "node")).
					WithDescriptor(string(profile.ResolvedKind()), profile.ResolvedApproval())
				rt.Sessions[name] = manager
			}
			return rt
		}
	}
}

func agentNames(cfg config.Config) []string {
	names := make([]string, 0, len(cfg.Agents)+1)
	seen := make(map[string]bool, len(cfg.Agents)+1)
	for name := range cfg.Agents {
		names = append(names, name)
		seen[name] = true
	}
	if fallback := cfg.Chat.Defaults.Agent; fallback != "" && !seen[fallback] {
		names = append(names, fallback)
	}
	return names
}

func approverFor(cfg config.Config, hooks agentruntime.Hooks) func(context.Context, string, string) (bool, string) {
	if len(hooks.Approvers) == 0 {
		return nil
	}
	if gate, ok := hooks.Approvers[cfg.Chat.Defaults.Agent]; ok {
		return gate.Approve
	}
	for _, gate := range hooks.Approvers {
		return gate.Approve
	}
	return nil
}

type nodeClient struct {
	host *Host
}

func (c *nodeClient) Initialize(ctx context.Context) error {
	if _, err := c.host.anyClient(); err != nil {
		return c.host.stranded(ctx, err)
	}
	return nil
}

func (c *nodeClient) NewSession(ctx context.Context, meta agent.SessionMetadata) (agent.Session, error) {
	chosen, err := c.host.delegate(ctx, meta)
	if err != nil {
		return agent.Session{}, err
	}
	session, err := chosen.node.client.NewSession(ctx, meta)
	if err != nil {
		return agent.Session{}, err
	}
	c.host.bindSession(session.ID, chosen.node)
	if chosen.takeover {
		c.host.markTakeover(session.ID, chosen.previous)
	}
	return session, nil
}

func (c *nodeClient) Prompt(ctx context.Context, sessionID string, req agent.PromptRequest) (<-chan agent.Event, error) {
	node, err := c.host.sessionNode(sessionID)
	if err != nil {
		return nil, err
	}
	return node.client.Prompt(ctx, sessionID, c.host.preparePrompt(sessionID, req))
}

func (c *nodeClient) Cancel(ctx context.Context, sessionID string) error {
	node, err := c.host.sessionNode(sessionID)
	if err != nil {
		return nil
	}
	return node.client.Cancel(ctx, sessionID)
}

func (c *nodeClient) Close() error { return nil }

func (c *nodeClient) CloseSession(sessionID string) {
	node, err := c.host.sessionNode(sessionID)
	c.host.unbindSession(sessionID)
	c.host.takeTakeover(sessionID)
	if err != nil {
		return
	}
	node.client.CloseSession(sessionID)
}

func (c *nodeClient) SupportsCancel(ctx context.Context) bool {
	client, err := c.host.anyClient()
	if err != nil {
		return true
	}
	return client.SupportsCancel(ctx)
}

var (
	_ agent.Client                                      = (*nodeClient)(nil)
	_ interface{ CloseSession(string) }                 = (*nodeClient)(nil)
	_ interface{ SupportsCancel(context.Context) bool } = (*nodeClient)(nil)
)

func (h *Host) pinnedNode(ctx context.Context, conversation agent.ConversationKey) (agentruntime.NodeRef, error) {
	ref := config.ConversationRef{TeamID: conversation.TeamID, ChannelID: conversation.ChannelID, ThreadTS: conversation.ThreadTS, DM: conversation.DM}
	if h.pins == nil || !ref.Valid() {
		return agentruntime.NodeRef{}, agentruntime.ErrNotPinned
	}
	pin, found, err := h.pins.Get(ctx, ref)
	if err != nil {
		return agentruntime.NodeRef{}, fmt.Errorf("read this conversation's node pin: %w", err)
	}
	if !found {
		return agentruntime.NodeRef{}, agentruntime.ErrNotPinned
	}
	node := pickByID(h.connected(), pin.NodeID)
	if node == nil {
		return agentruntime.NodeRef{NodeID: pin.NodeID}, fmt.Errorf("node %s, which this conversation runs on, is not connected", pin.NodeID)
	}
	return h.nodeRef(node), nil
}

func (h *Host) connectedNodes() []agentruntime.NodeRef {
	nodes := h.connected()
	out := make([]agentruntime.NodeRef, 0, len(nodes))
	for _, node := range nodes {
		out = append(out, h.nodeRef(node))
	}
	return out
}

func (h *Host) nodeRef(node *attached) agentruntime.NodeRef {
	return agentruntime.NodeRef{NodeID: node.nodeID, Owner: node.userID, Profiles: h.claimOf(node).Profiles}
}

func (h *Host) renewCredential(ctx context.Context, nodeID string) (agentruntime.RenewalStatus, error) {
	node := pickByID(h.connected(), nodeID)
	if node == nil {
		return "", fmt.Errorf("node %s is not connected", nodeID)
	}
	renewal, err := node.client.RenewCredential(ctx)
	if err != nil {
		return "", err
	}
	return agentruntime.RenewalStatus(renewal.Status), nil
}
