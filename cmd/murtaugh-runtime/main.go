// Command murtaugh-runtime is the runtime node daemon: the process that holds a
// connection open to a gateway and runs agents on this machine.
//
// It dials; the gateway never dials it. That is #170's attack-surface argument
// and it is why a node works from a laptop behind NAT with no tunnel and no
// inbound firewall rule: revoking a node is closing a socket, not chasing an
// address.
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
	"syscall"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/agentruntime/local"
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
var errNoGateway = errors.New("no gateway address: pass -gateway wss://host:port (a node dials in; the gateway never dials out)")

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
	configPath := fs.String("config", "", "path to config.yaml (default ~/.config/murtaugh/config.yaml)")
	gatewayURL := fs.String("gateway", "", "gateway address to attach to, ws:// or wss:// (required)")
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

	path := *configPath
	if path == "" {
		defaultPath, err := config.DefaultPath()
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
	if err := config.Bootstrap(path); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, cfgStore, err := configstore.Bootstrap(ctx, path, false)
	if err != nil {
		return err
	}
	defer func() { _ = cfgStore.Close() }()

	reportAgents(os.Stdout, cfg)
	if *gatewayURL == "" {
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
	served, name, err := serveAgent(cfg, logger, *agentName)
	if err != nil {
		return err
	}
	logger.Info("runtime node serving one agent", "agent", name, "gateway", *gatewayURL)

	// What this node claims, set BEFORE the first dial so the handshake answer
	// carries it. A node that advertised after attaching would be attached and
	// mute for a window, and the gateway builds its registry entry inside that
	// window.
	serving := []string{name}
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

	return attach(ctx, logger, attachment{
		gateway:    *gatewayURL,
		token:      token,
		insecure:   *insecure,
		client:     served.client,
		gate:       served.gate,
		background: served.background,
		tools:      served.tools,
		claim:      served.claim,
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
		return "", errors.New("no agents are configured on this node")
	}
	return "", errors.New("this node has several agents and no default: pass -agent to say which one it serves")
}

type attachment struct {
	gateway    string
	token      string
	insecure   bool
	client     agent.Client
	gate       *nodeserve.ToolGate
	background *nodeserve.BackgroundSink
	tools      *nodeserve.ToolProxy
	claim      *nodeserve.Advertiser
}

// attach dials the gateway and serves it, redialling until the process is
// stopped.
//
// A dropped connection is not fatal and is not announced: a laptop that sleeps
// at six o'clock disconnects every evening, and a node that gave up on the
// first refusal would need a human to restart it every morning.
func attach(ctx context.Context, logger *slog.Logger, a attachment) error {
	backoff := reconnectFloor
	for {
		conn, err := nodesocket.Dial(ctx, a.gateway, nodesocket.DialOptions{
			Token:              a.token,
			InsecureSkipVerify: a.insecure,
		})
		if err == nil {
			backoff = reconnectFloor
			logger.Info("attached to gateway", "gateway", a.gateway)
			err = nodeserve.Serve(ctx, conn, a.client, nodeserve.Options{
				Logger:      logger,
				Gate:        a.gate,
				Background:  a.background,
				Tools:       a.tools,
				Advertise:   a.claim,
				WindowBytes: nodesocket.DefaultWindowBytes,
				AckInterval: 30 * time.Second,
			})
			if err != nil {
				logger.Warn("gateway connection ended", "error", err)
			} else {
				logger.Info("gateway connection closed")
			}
		} else {
			logger.Warn("could not attach to gateway", "error", err, "retry_in", backoff)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(jitter(backoff)):
		}
		backoff *= 2
		if backoff > reconnectCeiling {
			backoff = reconnectCeiling
		}
	}
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
func reportAgents(out io.Writer, cfg config.Config) {
	fmt.Fprintf(out, "murtaugh-runtime %s\n", version)
	fmt.Fprintf(out, "config: %s\n", cfg.BaseDir)
	fmt.Fprintf(out, "node credential: %s\n", nodetoken.PathFor(cfg.BaseDir))
	if len(cfg.Agents) == 0 {
		fmt.Fprintln(out, "agents: none configured")
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
