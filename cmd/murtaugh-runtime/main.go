// Command murtaugh-runtime dials the gateway rather than being dialled, so a
// node needs no inbound port and revoking one is closing a socket.
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
	"github.com/miere/murtaugh/internal/credwarden"
	"github.com/miere/murtaugh/internal/help"
	"github.com/miere/murtaugh/internal/mcpbridge"
	"github.com/miere/murtaugh/internal/nodeclaim"
	"github.com/miere/murtaugh/internal/nodeserve"
	"github.com/miere/murtaugh/internal/nodesocket"
	"github.com/miere/murtaugh/internal/nodetoken"
	"github.com/miere/murtaugh/internal/tools"
	"github.com/miere/murtaugh/internal/tools/ask"
	authrequest "github.com/miere/murtaugh/internal/tools/auth/request"
	"github.com/miere/murtaugh/internal/tools/helptool"
	jobsrun "github.com/miere/murtaugh/internal/tools/jobs/run"
	"github.com/miere/murtaugh/internal/tools/ping"
	"github.com/miere/murtaugh/internal/tools/plan"
	versiontool "github.com/miere/murtaugh/internal/tools/version"
)

var version = "dev"

var errNoGateway = errors.New("no gateway address: set one with `murtaugh cfg node set --gateway wss://host:port` or pass -gateway (a node dials in; the gateway never dials out)")

const (
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
	if err := config.BootstrapNode(path); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, cfgStore, err := configstore.BootstrapRole(ctx, path, config.RoleNode, false)
	if err != nil {
		return err
	}
	defer func() { _ = cfgStore.Close() }()

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
	if _, err := nodetoken.ReadFile(credential); err != nil {
		return err
	}

	logger := newLogger(cfg.Access.Debug)
	runCtx, restart := context.WithCancel(ctx)
	defer restart()

	served, name, err := serveAgent(cfg, logger, *agentName)
	if err != nil {
		return err
	}
	var credentials *nodeserve.Credentials
	if warden := credwarden.New(credwarden.Options{Identities: servedIdentities(cfg, name), Logger: logger}); warden != nil {
		credentials = reportCredentials(warden, logger)
		go warden.Run(ctx)
	}
	repair := newRepairer(cfg, name, served.signIns, logger)

	var serving []string
	if name != "" {
		serving = []string{name}
		logger.Info("runtime node serving one agent", "agent", name, "gateways", strings.Join(seeds, " "))
	} else {
		logger.Warn("this node has no agent profile configured; attaching so its owner can be offered the setup form in Slack",
			"config", cfg.BaseDir, "gateways", strings.Join(seeds, " "))
	}
	served.claim.Publish(ctx, nodeclaim.Advertise(cfg, serving))

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
		tokenFile:  credential,
		insecure:   *insecure,
		client:     served.client,
		gate:       served.gate,
		background: served.background,
		signIns:    served.signIns,
		claim:      served.claim,
		configure:  configure.apply,
		restart:    restart,

		interruptible: served.interruptible,

		credentials: credentials,
		failed:      repair.failed,
		renew:       repair.renew,
	})
}

type servedAgent struct {
	client     agent.Client
	gate       *nodeserve.ToolGate
	background *nodeserve.BackgroundSink
	signIns    *nodeserve.SignIns
	claim      *nodeserve.Advertiser
	serveTools func(context.Context) error

	interruptible *bool
}

func serveAgent(cfg config.Config, logger *slog.Logger, requested string) (servedAgent, string, error) {
	name, err := chooseAgent(cfg, requested)
	if err != nil {
		return servedAgent{}, "", err
	}
	if name == "" {
		return servedAgent{
			client: nodeserve.UnconfiguredClient{},
			claim:  nodeserve.NewAdvertiser(logger),
		}, "", nil
	}

	gate := nodeserve.NewToolGate(logger)
	background := nodeserve.NewBackgroundSink(logger)
	signIns := nodeserve.NewSignIns(logger)
	claim := nodeserve.NewAdvertiser(logger)

	runtime := local.Builder(cfg, nodeTools(cfg, signIns), logger)(nodeHooks(cfg, gate, background))
	client, ok := runtime.Clients[name]
	if !ok {
		return servedAgent{}, "", fmt.Errorf("agent %q did not build; see the errors above", name)
	}
	return servedAgent{
		client:     client,
		gate:       gate,
		background: background,
		signIns:    signIns,
		claim:      claim,
		serveTools: runtime.ServeTools,

		interruptible: cfg.Agents[name].CancelOverride(),
	}, name, nil
}

func nodeTools(cfg config.Config, signIns *nodeserve.SignIns) *tools.Registry {
	registry := tools.NewRegistry()
	registry.Register(ping.New())
	registry.Register(versiontool.New(version))
	registry.Register(helptool.New(func() []help.Doc { return helpDocs(registry) }))
	registry.Register(ask.New(agent.TurnDisplay{}))
	registry.Register(plan.New(agent.TurnDisplay{}))
	registry.Register(authrequest.New(agent.TurnDisplay{}).WithoutConversation(signIns))

	// A job's reply goes nowhere but back to the caller here: only the gateway
	// reads report_to, and only for a run it scheduled itself.
	jobs := func(name string) (config.JobProfile, bool) {
		job, ok := cfg.Jobs[name]
		return job, ok
	}
	registry.Register(jobsrun.New(jobs).WithDelegator(local.Delegator(cfg, registry)))
	return registry
}

func helpDocs(registry *tools.Registry) []help.Doc {
	all := registry.All()
	docs := make([]help.Doc, 0, len(all))
	for _, t := range all {
		docs = append(docs, t)
	}
	return docs
}

func nodeHooks(cfg config.Config, gate *nodeserve.ToolGate, background *nodeserve.BackgroundSink) agentruntime.Hooks {
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
		return "", nil
	}
	return "", errors.New("this node has several agents and no default: pass -agent to say which one it serves")
}

type attachment struct {
	gateways   []string
	tokenFile  string
	insecure   bool
	client     agent.Client
	gate       *nodeserve.ToolGate
	background *nodeserve.BackgroundSink
	signIns    *nodeserve.SignIns
	claim      *nodeserve.Advertiser
	configure  func(context.Context, agentwire.NodeConfiguration) (agentwire.NodeConfigured, error)
	restart    func()

	credentials   *nodeserve.Credentials
	failed        func(error) error
	renew         func(context.Context) (agentwire.CredentialRenewal, error)
	interruptible *bool

	dial func(ctx context.Context, address, token string) (*nodesocket.Conn, error)
	wait func(d time.Duration) <-chan time.Time
}

func (a attachment) dialer() func(context.Context, string, string) (*nodesocket.Conn, error) {
	if a.dial != nil {
		return a.dial
	}
	return func(ctx context.Context, address, token string) (*nodesocket.Conn, error) {
		return nodesocket.Dial(ctx, address, nodesocket.DialOptions{
			Token:              token,
			InsecureSkipVerify: a.insecure,
		})
	}
}

func (a attachment) waiter() func(time.Duration) <-chan time.Time {
	if a.wait != nil {
		return a.wait
	}
	return time.After
}

func attach(ctx context.Context, logger *slog.Logger, a attachment) error {
	gateways := newGatewayList(a.gateways...)
	gateways.credential = a.tokenFile
	dial, wait := a.dialer(), a.waiter()
	backoff := reconnectFloor
	for {
		address := gateways.current()
		token, err := nodetoken.ReadFile(a.tokenFile)
		if err != nil {
			logger.Error("could not read this node's credential; trying again shortly", "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-wait(jitter(backoff)):
			}
			backoff = nextBackoff(backoff)
			continue
		}
		conn, err := dial(ctx, address, token)
		if err != nil {
			if gateways.refusal(address, err, logger) {
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
			SignIns:     a.signIns,
			Credentials: a.credentials,
			Failed:      a.failed,
			Advertise:   a.claim,
			Configure:   a.configure,
			Restart:     a.restart,
			WindowBytes: nodesocket.DefaultWindowBytes,
			AckInterval: 30 * time.Second,

			RenewCredential: a.renew,
			Interruptible:   a.interruptible,
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

func nextBackoff(backoff time.Duration) time.Duration {
	backoff *= 2
	if backoff > reconnectCeiling {
		return reconnectCeiling
	}
	return backoff
}

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
