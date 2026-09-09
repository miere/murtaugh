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
			// The gateway's access policy carries the node GRANTS delegation
			// reads, and a reload is the only way one is ever added, so it is
			// re-bound here rather than captured when the Host was built.
			host.setAccess(cfg.Access)
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

			rt := agentruntime.Runtime{
				// Headless dispatch (#199): jobs, workflow triggers and unfurls
				// on the MAIN node. It is set here, ABOVE the chat check below,
				// and that placement is load-bearing — agentruntime.Hooks says
				// these surfaces run whether or not chat is enabled, and a
				// gateway with chat off is precisely the deployment that exists
				// to run scheduled jobs.
				Delegator: &headlessDelegator{
					host:        host,
					idleTimeout: cfg.Defaults.EffectiveRequestTimeout(),
					log:         logger.With("runtime", "node", "delegate", "headless"),
				},
			}
			if !hooks.Chat {
				// No chat surface means nothing would ever prompt the session
				// managers, so none is built.
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

// nodeClient is agent.Client over the fleet, with delegation underneath it.
//
// It is a thin indirection rather than the remote client itself because the
// session managers outlive any one connection: a node that drops and dials back
// in must be picked up by the managers that are already there, and a captured
// client would be a dead one. With a registry behind it the indirection carries
// the second thing #196 needs — which of several nodes this conversation is on
// — and that is why the choice lives here rather than in the gateway. It is
// under *agent.SessionManager, not in place of it, so the four capability
// surfaces the gateway type-asserts on the MANAGER are untouched.
type nodeClient struct {
	host *Host
}

// Initialize answers for the fleet.
//
// The real initialize already happened at each node's handshake, and it had to:
// the session manager latches "initialized" on first success and would never
// repeat it for a node that connected afterwards. What is left here is the
// question the manager is really asking — is there anything to talk to.
func (c *nodeClient) Initialize(context.Context) error {
	_, err := c.host.anyClient()
	return err
}

// NewSession elects the node this conversation runs on, and binds the session
// it opens to that node.
//
// This is delegation's one entry point on the turn path, and it is reached only
// when the session manager has no live session for the conversation — a cold
// conversation, an idle-evicted one, or one whose node has just gone. So an
// election is once per session and not once per message, and the pin store sees
// one read and at most one write in the same place.
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
		// Recorded against the SESSION rather than the conversation because the
		// notice belongs to the first prompt this session serves, and a session
		// is what Prompt is handed. It is consumed once; every later turn on
		// this node is an ordinary one.
		c.host.markTakeover(session.ID, chosen.previous)
	}
	return session, nil
}

// Prompt sends the turn to the node holding the session.
//
// It never re-elects. A session id names a machine, so honouring the pin here
// would be honouring it twice — and disagreeing with the binding would send a
// node a session id it never minted, which it answers with an error the user
// sees.
func (c *nodeClient) Prompt(ctx context.Context, sessionID string, req agent.PromptRequest) (<-chan agent.Event, error) {
	node, err := c.host.sessionNode(sessionID)
	if err != nil {
		// agent.ErrSessionGone, which the session manager answers by discarding
		// the binding and opening a fresh session — and THAT re-runs the
		// election, overwrites the stale pin, and marks the takeover. #170's
		// position is that a dropped node loses its sessions; what it must not
		// lose is the conversation.
		return nil, err
	}
	return node.client.Prompt(ctx, sessionID, c.host.preparePrompt(sessionID, req))
}

func (c *nodeClient) Cancel(ctx context.Context, sessionID string) error {
	node, err := c.host.sessionNode(sessionID)
	if err != nil {
		// Nothing is running, so nothing failed to be interrupted. Reporting an
		// error here would put a failure card on the idle path's five-second
		// cancel for a node that is simply gone.
		return nil
	}
	return node.client.Cancel(ctx, sessionID)
}

// Close releases the gateway's side of the conversation, not the node. The node
// is a separate process with its own lifetime; a gateway shutting down closes
// its connection, which the Host owns.
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
