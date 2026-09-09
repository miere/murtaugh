// Command murtaugh-runtime is the runtime node daemon: the process that holds a
// connection open to a gateway and runs agents on this machine.
//
// It dials; the gateway never dials it. That is #170's attack-surface argument
// and it is why a node works from a laptop behind NAT with no tunnel and no
// inbound firewall rule: revoking a node is closing a socket, not chasing an
// address.
//
// # Which gateway, and why it did not attach
//
// Dialling in means this process owns finding the gateway, which after a
// failover is a different machine. A gateway that is not the elected one
// redirects rather than dropping the connection, and the addresses it names are
// ADDED to the configured seeds — never substituted for them, or a node asleep
// through a topology change wakes holding only addresses that no longer exist.
// gateways.go owns that list and the rule. The seeds come from this node's own
// configuration (`node.gateway`) or from -gateway, which overrides it.
//
// # A node that has never been configured
//
// It still attaches, advertising nothing. That empty claim is #170 Change I's
// onboarding trigger: the gateway learns who OWNS the node from the credential
// it presented, offers that person the existing Slack setup form, and sends the
// answers back down this connection for configure.go to apply. A node that
// refused to start without a profile — which is what this did before item 12 —
// made the trigger unreachable, because the one node that needed onboarding was
// the one node that could never connect to ask for it.
//
// It also owns the distinction #197 exists for: a failed dial is reported as
// "wrong gateway", "gateway down" or "credential rejected", because those are
// three different problems with three different owners and a single "could not
// attach" line names none of them.
//
// Unlike cmd/murtaugh-gateway, this binary MAY reach the agent packages — it is
// the half of the split whose whole job is running a model.
//
// # Murtaugh's own tools reach this node over the link
//
// A node's agent gets Murtaugh's tools — the ones the gateway will let it have —
// through nodeserve.ToolProxy: a registry of stand-ins whose Invoke is one
// round trip over the connection this process already holds open. The gateway
// decides what is in that registry; see internal/toolset's partition and
// internal/nodehost, which is the one place it is enforced.
//
// Two consequences worth knowing before reading the wiring below. The proxy is
// built BEFORE the runtime, because both backend families latch: a native agent
// resolves its toolset at its first Initialize, and an acp/claude_code agent's
// aggregator resolves it when its first session is registered. The gateway
// handshake fills the proxy ahead of both; a registry filled after either is one
// that backend never looks at again. And ServeTools is started here, which it
// was not before: an
// acp/claude_code agent reaches tools through a local MCP aggregator socket, and
// nothing on a node was binding it — so those two backends were not merely
// tool-less, they were spawning a bridge subprocess against a socket nobody was
// listening on.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/agentruntime/local"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/migrate"
	configstore "github.com/miere/murtaugh/internal/config/store"
	"github.com/miere/murtaugh/internal/mcpbridge"
	"github.com/miere/murtaugh/internal/nodeclaim"
	"github.com/miere/murtaugh/internal/nodeserve"
	"github.com/miere/murtaugh/internal/nodesocket"
	"github.com/miere/murtaugh/internal/nodetoken"
)

var version = "dev"

// errNoGateway is what this binary exits with when it is given nothing to dial.
// It is an error rather than a clean exit so a supervisor pointed at a node
// that was never told where its gateway is reports a failure instead of
// flapping a process that silently does nothing.
var errNoGateway = errors.New("no gateway address: set one with `murtaugh cfg node set --gateway wss://host:port` or pass -gateway (a node dials in; the gateway never dials out)")

const (
	// reconnectFloor and reconnectCeiling bound the redial backoff. Jittered,
	// because every node discovers a dead gateway at the same moment and an
	// unjittered fleet redials in lockstep.
	reconnectFloor   = 1 * time.Second
	reconnectCeiling = 30 * time.Second
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "murtaugh-runtime:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// `murtaugh-runtime mcp-bridge` is the transparent stdio↔socket proxy an
	// acp/claude_code agent spawns to reach this node's MCP aggregator. The
	// aggregator advertises os.Executable() as the command, which on a node is
	// THIS binary — so without this branch the agent spawns a subprocess that
	// exits immediately with "unexpected argument", every session, and the
	// symptom is an agent with no Murtaugh tools and nothing in any log that
	// names the cause. Dispatched before any config or logging is wired,
	// because stdout belongs to MCP.
	if len(args) > 0 && args[0] == mcpbridge.Subcommand {
		return runMCPBridge()
	}

	fs := flag.NewFlagSet("murtaugh-runtime", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to this node's config.yaml (default ~/.config/murtaugh/node/config.yaml)")
	gatewayURL := fs.String("gateway", "", "gateway address to attach to, ws:// or wss://; overrides node.gateway in the configuration. Addresses learned from a redirect are added to the seeds and never replace them")
	agentName := fs.String("agent", "", "which configured agent this node serves (default: the chat default, or the only one)")
	tokenPath := fs.String("token-file", "", "path to this node's credential (default: node-token beside the config)")
	insecure := fs.Bool("insecure-skip-verify", false, "do not verify the gateway's TLS certificate")
	showVersion := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q: this binary takes no subcommands", fs.Arg(0))
	}

	// A node's own configuration ROOT, not the gateway's. Two roles in one
	// directory share a .env, a store and a node-token — and share
	// internal/config/migrate's backup/restore, which reverts every top-level
	// file in the directory it runs in. See config.DefaultNodePath.
	path := *configPath
	if path == "" {
		defaultPath, err := config.DefaultNodePath()
		if err != nil {
			return err
		}
		path = defaultPath
	}

	if applied, err := migrate.Run(filepath.Dir(path)); err != nil {
		return fmt.Errorf("config migration failed: %w", err)
	} else if len(applied) > 0 {
		fmt.Fprintf(os.Stderr, "murtaugh-runtime: migrated config to schema v%d\n", applied[len(applied)-1])
	}
	// The node's skeleton, which unlike the gateway's carries no `oauth:` block:
	// a node has no Slack connection and must never hold the workspace's tokens.
	if err := config.BootstrapNode(path); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// RoleNode, which is what lets this load at all: a node's configuration is
	// validated without the Slack credentials it must not hold. See
	// internal/config/role.go.
	cfg, cfgStore, err := configstore.BootstrapRole(ctx, path, config.RoleNode, false)
	if err != nil {
		return err
	}
	defer func() { _ = cfgStore.Close() }()

	// The seed addresses: the flag, or the node's own configuration. The flag
	// wins so an operator can point a node somewhere once without editing it.
	seeds := cfg.Node.Seeds()
	if flagged := strings.TrimSpace(*gatewayURL); flagged != "" {
		seeds = []string{flagged}
	}

	reportAgents(os.Stdout, cfg, seeds)
	if len(seeds) == 0 {
		return errNoGateway
	}

	credential := *tokenPath
	if credential == "" {
		credential = nodetoken.PathFor(cfg.BaseDir)
	}
	token, err := nodetoken.ReadFile(credential)
	if err != nil {
		return err
	}

	logger := newLogger(cfg.Access.Debug)
	// The process context, which the configuration applier below can end. A node
	// that has just been given its first agent profiles restarts into them —
	// both backend families latch their toolset at construction, so a process
	// built with nothing cannot grow an agent. See configure.go.
	runCtx, restart := context.WithCancel(ctx)
	defer restart()

	served, name, err := serveAgent(cfg, logger, *agentName)
	if err != nil {
		return err
	}

	// What this node claims, set BEFORE the first dial so the handshake answer
	// carries it. A node that advertised after attaching would be attached and
	// mute for a window, and the gateway builds its registry entry inside that
	// window.
	//
	// A node with nothing configured advertises NOTHING, and that empty claim is
	// the signal: #170 Change I makes it the trigger for onboarding the node's
	// owner through Slack, which the gateway drives because this process has no
	// Slack of its own. It is why such a node attaches at all rather than
	// refusing to start — an unpublished node has no owner the gateway can ask.
	var serving []string
	if name != "" {
		serving = []string{name}
		logger.Info("runtime node serving one agent", "agent", name, "gateways", strings.Join(seeds, " "))
	} else {
		logger.Warn("this node has no agent profile configured; attaching so its owner can be offered the setup form in Slack",
			"config", cfg.BaseDir, "gateways", strings.Join(seeds, " "))
	}
	served.claim.Publish(ctx, nodeclaim.Advertise(cfg, serving))

	// And the only reason a node ever changes its claim afterwards. Nothing
	// else on this binary notices a configuration edit at all: the agent is
	// built once from the snapshot above and stays built, because both backend
	// families latch their toolset and the redial loop reuses the client it
	// captured. This re-reads the claim and nothing else.
	//
	// A watcher that cannot be built is reported and skipped rather than fatal:
	// a node that cannot notice an edit still serves every conversation it is
	// given, and one that refused to start serves none.
	if watcher, err := nodeclaim.NewWatcher(ctx, nodeclaim.Options{
		Store:   cfgStore,
		Base:    cfg,
		Serving: serving,
		Publish: served.claim.Publish,
		Logger:  logger,
	}); err != nil {
		logger.Warn("this node will not notice configuration changes", "error", err)
	} else {
		go watcher.Run(ctx)
	}

	// The node's own MCP aggregator socket, which an acp/claude_code agent's
	// bridge subprocess dials. It is local to this machine and never crosses the
	// network: the tool CALLS cross, one frame each, not the MCP byte stream —
	// which is what keeps a reconnect from leaving a half-initialised MCP
	// session on the far side (#170 Concern 5).
	//
	// A failure here degrades those two backends and does not stop the node: a
	// native agent needs no aggregator, and a node that refused to start over it
	// would take chat down for an agent that never used it.
	if served.serveTools != nil {
		go func() {
			if err := served.serveTools(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error("the local tool aggregator stopped; acp and claude_code agents on this node will have no Murtaugh tools",
					"error", err)
			}
		}()
	}

	configure := &configurer{store: cfgStore, baseDir: cfg.BaseDir, logger: logger, restarts: true}
	return attach(runCtx, logger, attachment{
		gateways:   seeds,
		token:      token,
		insecure:   *insecure,
		client:     served.client,
		gate:       served.gate,
		background: served.background,
		tools:      served.tools,
		claim:      served.claim,
		configure:  configure.apply,
		// Fired by nodeserve AFTER the answer is on the wire, never by the
		// applier: it cancels the context this very connection is served on.
		restart: restart,
	})
}

// servedAgent is the one agent this node offers, the gate its tool calls go
// through, and the sink its background events leave by.
type servedAgent struct {
	client     agent.Client
	gate       *nodeserve.ToolGate
	background *nodeserve.BackgroundSink
	tools      *nodeserve.ToolProxy
	// claim holds what this node tells the gateway it serves. Like the three
	// above it is built before the agent and bound to whichever connection is
	// serving; unlike them it is also read at one specific moment, when the
	// handshake answer is assembled.
	claim *nodeserve.Advertiser
	// serveTools binds this node's LOCAL aggregator socket, the one an
	// acp/claude_code agent's bridge subprocess dials. nil when there is nothing
	// to serve.
	serveTools func(context.Context) error
}

// serveAgent builds the node's in-process runtime and picks the agent it will
// serve.
//
// One agent, because the protocol carries no agent name: a link IS an agent.
// That is #193's deliberate simplification — the registry that lets a node
// advertise what it can serve is item 9 — and it is named here rather than
// discovered later from a confusing failure.
func serveAgent(cfg config.Config, logger *slog.Logger, requested string) (servedAgent, string, error) {
	name, err := chooseAgent(cfg, requested)
	if err != nil {
		return servedAgent{}, "", err
	}
	if name == "" {
		// Nothing configured. The node attaches anyway, with an empty
		// advertisement, because that empty claim is what triggers onboarding
		// its owner through Slack — see the call site, and #170 Change I. None
		// of the collaborators below are built: there is no agent for them to
		// serve, and the gate, the proxy and the aggregator all exist to feed
		// one. The claim advertiser is the exception, because the empty claim is
		// the entire point.
		return servedAgent{
			client: nodeserve.UnconfiguredClient{},
			claim:  nodeserve.NewAdvertiser(logger),
		}, "", nil
	}

	// All three collaborators are built before the agent because the backends
	// want them at construction, and bound to a connection when one arrives.
	// The proxy especially: its registry is what the agent's toolset is resolved
	// from, and both backend families latch that resolution — native at its
	// first Initialize, acp/claude_code when their aggregator registers its first
	// session. The registry handed over here is EMPTY; the handshake fills it
	// before either latch, which is the whole reason the aggregator resolves
	// lazily rather than at construction.
	gate := nodeserve.NewToolGate(logger)
	background := nodeserve.NewBackgroundSink(logger)
	proxy := nodeserve.NewToolProxy(logger)
	claim := nodeserve.NewAdvertiser(logger)

	runtime := local.Builder(cfg, proxy.Registry(), logger)(nodeHooks(cfg, gate, background))
	client, ok := runtime.Clients[name]
	if !ok {
		return servedAgent{}, "", fmt.Errorf("agent %q did not build; see the errors above", name)
	}
	return servedAgent{
		client:     client,
		gate:       gate,
		background: background,
		tools:      proxy,
		claim:      claim,
		serveTools: runtime.ServeTools,
	}, name, nil
}

// nodeHooks is everything a node contributes to its own in-process runtime.
//
// These are the collaborators only a Slack-facing process can supply, which on
// a node means: reached over the link instead of in memory. Both must be
// present or the corresponding feature fails SILENTLY, one hop before the
// protocol could carry it — an unset Approver runs side-effecting tools
// unprompted, and an unset BackgroundEvents makes claude_code drop a background
// stretch's events at the backend, so the gateway's "went quiet" notice never
// appears with nothing logged on the side anyone would debug.
func nodeHooks(cfg config.Config, gate *nodeserve.ToolGate, background *nodeserve.BackgroundSink) agentruntime.Hooks {
	// Every agent gets the gate: which one this node serves is decided
	// elsewhere, and an agent built with no gate would run side-effecting tools
	// unprompted if that choice changed.
	approvers := make(map[string]agentruntime.Approver, len(cfg.Agents))
	for agentName := range cfg.Agents {
		approvers[agentName] = gate
	}
	return agentruntime.Hooks{
		Chat:             true,
		Approvers:        approvers,
		BackgroundEvents: background.Handle,
	}
}

func chooseAgent(cfg config.Config, requested string) (string, error) {
	if requested != "" {
		if _, ok := cfg.Agents[requested]; !ok {
			return "", fmt.Errorf("no agent named %q is configured on this node", requested)
		}
		return requested, nil
	}
	if fallback := cfg.Chat.Defaults.Agent; fallback != "" {
		if _, ok := cfg.Agents[fallback]; ok {
			return fallback, nil
		}
	}
	if len(cfg.Agents) == 1 {
		for name := range cfg.Agents {
			return name, nil
		}
	}
	if len(cfg.Agents) == 0 {
		// The empty name, not an error. A node that has never been configured
		// must be able to attach, because the gateway learns who OWNS it from
		// the credential the connection presents and learns it has nothing from
		// the empty advertisement — and #170 Change I makes those two facts
		// together the trigger for onboarding the owner through Slack. Refusing
		// to start, which is what this did before, made that trigger unreachable
		// by construction: the one node that needs onboarding was the one node
		// that could never connect to ask for it.
		return "", nil
	}
	return "", errors.New("this node has several agents and no default: pass -agent to say which one it serves")
}

type attachment struct {
	gateways   []string
	token      string
	insecure   bool
	client     agent.Client
	gate       *nodeserve.ToolGate
	background *nodeserve.BackgroundSink
	tools      *nodeserve.ToolProxy
	claim      *nodeserve.Advertiser
	configure  func(context.Context, agentwire.NodeConfiguration) (agentwire.NodeConfigured, error)
	restart    func()

	// dial and wait are the loop's two seams, nil in every binary and set only
	// by the loop's own test.
	//
	// They exist because two of the behaviours #197 headlines are properties of
	// the LOOP rather than of gatewayList: that a hop costs no backoff, and
	// that paying a backoff returns the hop budget. Both are invisible to a
	// test that drives gatewayList directly — it reaches past the caller — and
	// unreachable through the real ones, which need a gateway on a socket and a
	// wall clock willing to spend thirty seconds.
	dial func(ctx context.Context, address string) (*nodesocket.Conn, error)
	wait func(d time.Duration) <-chan time.Time
}

// dialer is the real dial, closing over the credential this node presents.
func (a attachment) dialer() func(context.Context, string) (*nodesocket.Conn, error) {
	if a.dial != nil {
		return a.dial
	}
	return func(ctx context.Context, address string) (*nodesocket.Conn, error) {
		return nodesocket.Dial(ctx, address, nodesocket.DialOptions{
			Token:              a.token,
			InsecureSkipVerify: a.insecure,
		})
	}
}

// waiter is the real backoff wait.
func (a attachment) waiter() func(time.Duration) <-chan time.Time {
	if a.wait != nil {
		return a.wait
	}
	return time.After
}

// attach dials the gateway and serves it, redialling until the process is
// stopped.
//
// A dropped connection is not fatal and is not announced: a laptop that sleeps
// at six o'clock disconnects every evening, and a node that gave up on the
// first refusal would need a human to restart it every morning.
//
// Which address it dials is gatewayList's business, and why it waited — or did
// not — is gatewayList.refusal's. Both live in gateways.go, because the loop
// below is the part that must stay obvious.
func attach(ctx context.Context, logger *slog.Logger, a attachment) error {
	gateways := newGatewayList(a.gateways...)
	dial, wait := a.dialer(), a.waiter()
	backoff := reconnectFloor
	for {
		address := gateways.current()
		conn, err := dial(ctx, address)
		if err != nil {
			if gateways.refusal(address, err, logger) {
				// A redirect, and somewhere to go. Hop now: the fleet answered
				// and named the leader, so waiting out a backoff earned by
				// unrelated failures would idle a healthy node for nothing.
				select {
				case <-ctx.Done():
					return nil
				default:
				}
				continue
			}
			select {
			case <-ctx.Done():
				return nil
			case <-wait(jitter(backoff)):
			}
			// The wait is what the hop budget was protecting against spending;
			// having paid it, the node may follow redirects again.
			gateways.waited()
			backoff = nextBackoff(backoff)
			continue
		}

		backoff = reconnectFloor
		gateways.attached()
		logger.Info("attached to gateway", "gateway", address)
		if err := nodeserve.Serve(ctx, conn, a.client, nodeserve.Options{
			Logger:      logger,
			Gate:        a.gate,
			Background:  a.background,
			Tools:       a.tools,
			Advertise:   a.claim,
			Configure:   a.configure,
			Restart:     a.restart,
			WindowBytes: nodesocket.DefaultWindowBytes,
			AckInterval: 30 * time.Second,
		}); err != nil {
			logger.Warn("gateway connection ended", "error", err, "gateway", address)
		} else {
			logger.Info("gateway connection closed", "gateway", address)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-wait(jitter(backoff)):
		}
		backoff = nextBackoff(backoff)
	}
}

// nextBackoff doubles the wait, up to the ceiling.
func nextBackoff(backoff time.Duration) time.Duration {
	backoff *= 2
	if backoff > reconnectCeiling {
		return reconnectCeiling
	}
	return backoff
}

// runMCPBridge runs the `murtaugh-runtime mcp-bridge` subcommand: a transparent
// pipe between the spawning agent's stdio and this node's own aggregator socket.
// It is byte-for-byte the same job `murtaugh mcp-bridge` does, and it stays a
// local hop — the socket is on this machine, and it is the tool CALLS that cross
// the network, one frame each.
//
// The socket path and session token arrive via the environment, so there is no
// argument parsing to collide with this binary's flags.
func runMCPBridge() error {
	socket := os.Getenv(mcpbridge.EnvSocket)
	token := os.Getenv(mcpbridge.EnvToken)
	if socket == "" || token == "" {
		return fmt.Errorf("%s requires %s and %s in the environment", mcpbridge.Subcommand, mcpbridge.EnvSocket, mcpbridge.EnvToken)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return mcpbridge.RunBridge(ctx, socket, token, os.Stdin, os.Stdout)
}

// jitter spreads a fleet's reconnections over the window rather than firing
// them together.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1)) //nolint:gosec // scheduling jitter, not a secret
}

func newLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

// reportAgents prints what this node would serve, and where the credential it
// presents lives. It is printed before anything is dialled, because "is this
// machine configured to be a node at all?" is the first question an operator
// asks and the one a connection failure does not answer.
func reportAgents(out io.Writer, cfg config.Config, seeds []string) {
	fmt.Fprintf(out, "murtaugh-runtime %s\n", version)
	fmt.Fprintf(out, "config: %s\n", cfg.BaseDir)
	fmt.Fprintf(out, "node credential: %s\n", nodetoken.PathFor(cfg.BaseDir))
	if len(seeds) == 0 {
		fmt.Fprintln(out, "gateways: none configured")
	} else {
		fmt.Fprintf(out, "gateways: %s\n", strings.Join(seeds, " "))
	}
	if len(cfg.Agents) == 0 {
		fmt.Fprintln(out, "agents: none configured — attaching so this node's owner can be offered the setup form in Slack")
		return
	}
	names := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Fprintf(out, "agents: %d\n", len(names))
	for _, name := range names {
		fmt.Fprintf(out, "  %s (%s)\n", name, cfg.Agents[name].ResolvedKind())
	}
}
