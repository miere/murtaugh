package nodehost

import (
	"context"
	"log/slog"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/tools"
)

// Runtime is the agentruntime.Builder a broker gateway uses: session managers
// whose agent.Client is whichever node is attached.
//
// It builds one manager per agent name the gateway knows about, all over the
// same node, because the protocol carries no agent identity yet — see the
// package doc. A gateway with no agent profiles of its own still gets the
// default chat agent, so a node can serve a gateway that holds nothing but
// routing.
//
// It contributes nothing that can run a model, which is the point: this is the
// builder cmd/murtaugh-gateway may link, and CI checks that its dependency
// closure stays clear of the backend packages. The REGISTRY it is handed is not
// a backend and never was — it is the tool surface a node's agent reaches over
// the tool channel (#194), and until item 8 this builder threw it away, which is
// why a node's agent had no Murtaugh tools at all.
func Runtime(host *Host) func(config.Config, *tools.Registry, *slog.Logger) agentruntime.Builder {
	return func(cfg config.Config, registry *tools.Registry, logger *slog.Logger) agentruntime.Builder {
		if logger == nil {
			logger = slog.Default()
		}
		return func(hooks agentruntime.Hooks) agentruntime.Runtime {
			host.SetApprover(approverFor(cfg, hooks))
			// Bound here because this is where the gateway hands its registry
			// over, and a reload runs this builder again while the node
			// connection survives. It re-binds the same pointer: the registry is
			// built once, in app.New. See SetTools for the staleness that
			// actually follows from that, which is not this one.
			host.SetTools(registry)
			// Background events belong to the gateway that is currently serving,
			// and a reload replaces it, so the sink is refreshed here rather
			// than captured when the connection was made.
			host.setBackground(hooks.BackgroundEvents)

			rt := agentruntime.Runtime{}
			if !hooks.Chat {
				// No chat surface means nothing would ever prompt these. There
				// is deliberately no Delegator either: a job or an unfurl
				// running on a node is #170's item 13, and a nil delegator is
				// already reported as "agent delegation is unavailable" rather
				// than silently doing nothing.
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

// agentNames is every name a conversation could route to.
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

// approverFor picks the gate a node's native agent reaches.
//
// One node serves one agent, so one gate is reachable. The default chat agent's
// is chosen because that is the one a conversation lands on when nothing more
// specific matched; any other choice would be arbitrary in the same way and
// harder to explain. When the registry carries agent identity this becomes a
// lookup rather than a choice.
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

// nodeClient is agent.Client bound to whichever node is attached.
//
// It is a thin indirection rather than the remote client itself because the
// session managers outlive any one connection: a node that drops and dials back
// in must be picked up by the managers that are already there, and a captured
// client would be a dead one.
type nodeClient struct {
	host *Host
}

// Initialize answers for the attached node.
//
// The real initialize already happened at the handshake, and it had to: the
// session manager latches "initialized" on first success and would never repeat
// it for a node that connected afterwards. What is left here is the question
// the manager is really asking — is there anything to talk to.
func (c *nodeClient) Initialize(context.Context) error {
	_, err := c.host.client()
	return err
}

func (c *nodeClient) NewSession(ctx context.Context, meta agent.SessionMetadata) (agent.Session, error) {
	client, err := c.host.client()
	if err != nil {
		return agent.Session{}, err
	}
	return client.NewSession(ctx, meta)
}

func (c *nodeClient) Prompt(ctx context.Context, sessionID string, req agent.PromptRequest) (<-chan agent.Event, error) {
	client, err := c.host.client()
	if err != nil {
		return nil, err
	}
	// A session id minted by a node that has since been replaced means nothing
	// to the newcomer, and the node answers with an error the user sees. That
	// is #170's stated position — a dropped node loses its sessions, and the
	// recovery path is the takeover card (#196), not a silent re-creation that
	// would look to the user like the agent forgetting the conversation.
	return client.Prompt(ctx, sessionID, req)
}

func (c *nodeClient) Cancel(ctx context.Context, sessionID string) error {
	client, err := c.host.client()
	if err != nil {
		// Nothing is running, so nothing failed to be interrupted. Reporting an
		// error here would put a failure card on the idle path's five-second
		// cancel for a node that is simply gone.
		return nil
	}
	return client.Cancel(ctx, sessionID)
}

// Close releases the gateway's side of the conversation, not the node. The node
// is a separate process with its own lifetime; a gateway shutting down closes
// its connection, which the Host owns.
func (c *nodeClient) Close() error { return nil }

func (c *nodeClient) CloseSession(sessionID string) {
	client, err := c.host.client()
	if err != nil {
		return
	}
	client.CloseSession(sessionID)
}

func (c *nodeClient) SupportsCancel(ctx context.Context) bool {
	client, err := c.host.client()
	if err != nil {
		// Unknown degrades to interruptible, the same way the session manager's
		// own unresolved probe does: a missing answer must never be the one that
		// silently stops interrupts from working.
		return true
	}
	return client.SupportsCancel(ctx)
}

var (
	_ agent.Client                                      = (*nodeClient)(nil)
	_ interface{ CloseSession(string) }                 = (*nodeClient)(nil)
	_ interface{ SupportsCancel(context.Context) bool } = (*nodeClient)(nil)
)
