// Command murtaugh-gateway is the Slack gateway as its own binary.
//
// It is the same daemon `murtaugh slack gateway` runs — same composition root,
// same leader election, same configuration reload — with one difference that is
// the entire point of it: it links no agent machinery. There is no
// internal/agentruntime/local here, so the process cannot build a backend, and
// `go list -deps` over this package cannot reach internal/agentbuild,
// internal/llm or any of internal/agent/{acp,native,claudecode}. CI checks that
// on every change (see .github/workflows/ci.yml, "Gateway reachability rule").
//
// With no -node-listen it is a gateway that answers Slack but has no agent to
// give a conversation to: chat reports no agent, and delegate-to-agent surfaces
// report that delegation is unavailable. Given one, it opens the daemon's first
// inbound listener, accepts one authenticated runtime node, and hands every
// conversation to it — #170 item 7, the new path working but not the default.
//
// The flag defaults to empty and there is deliberately no configuration key for
// it. Binding a port is a new attack surface on the machine, and it must not be
// possible to acquire one by editing a config file that `murtaugh slack
// gateway` — the shipping default, which binds nothing — also reads.
//
// # Its configuration is the gateway's half, and only that
//
// It loads with config.RoleGateway: Slack tokens, access, election, rules and
// pins, with the agent profile bodies belonging to whichever node serves them.
// The visible consequence is that the default agent NAME is no longer checked
// against a BODY when it is written — this process holds no bodies — so the
// check happens when a node attaches, against what that user's fleet advertises.
// "Is my configuration valid" therefore depends partly on who is online, and the
// answer arrives in the journal on the gateway stream rather than at startup. A
// gateway that refused to boot on an empty registry could never boot at all.
//
// The listener binds at process start; whether it ACCEPTS is decided by the
// election, wired into the Host inside the daemon's run. A standby holds the
// port and turns nodes away with the leader's address, which is why the two are
// separate: a listener that came and went with leadership would have to
// re-acquire its port at exactly the moment a failover is already going badly.
// -node-advertise is what a deployment behind a reverse proxy tells nodes to
// come to, since this process cannot discover its own public name.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/app"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/migrate"
	configstore "github.com/miere/murtaugh/internal/config/store"
	"github.com/miere/murtaugh/internal/nodehost"
	"github.com/miere/murtaugh/internal/tools"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "murtaugh-gateway:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("murtaugh-gateway", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config.yaml (default ~/.config/murtaugh/config.yaml)")
	nodeListen := fs.String("node-listen", "", "address to accept runtime node connections on, e.g. 127.0.0.1:8787 (empty: accept none)")
	nodeAdvertise := fs.String("node-advertise", "", "address(es) nodes should use to reach this gateway, space- or comma-separated and used verbatim, e.g. \"wss://gateway.example.com wss://192.0.2.10:8443\" (empty: work it out from the listener, which yields ws:// and is only dialable on loopback)")
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

	// The same startup `murtaugh slack gateway` runs: migrate a legacy config
	// directory, seed a fresh one, then resolve the running config out of the
	// store. Each migration step is backup/validate/rollback-guarded.
	if applied, err := migrate.Run(filepath.Dir(path)); err != nil {
		return fmt.Errorf("config migration failed: %w", err)
	} else if len(applied) > 0 {
		fmt.Fprintf(os.Stderr, "murtaugh-gateway: migrated config to schema v%d\n", applied[len(applied)-1])
	}
	if err := config.Bootstrap(path); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// RoleGateway: this half holds the Slack credentials and no agent profile
	// bodies, so a name like chat.defaults.agent can no longer be resolved
	// against one here. That check moves to CONNECT time, against the profiles
	// the attaching user's fleet advertises — see internal/config/role.go, and
	// the "is my configuration valid now depends on who is online" note there.
	cfg, cfgStore, err := configstore.BootstrapRole(ctx, path, config.RoleGateway, false)
	if err != nil {
		return err
	}
	defer func() { _ = cfgStore.Close() }()

	logger := newLogger(cfg.Access.Debug)
	store, recorder, closeJournal := app.OpenJournal(cfg, logger)
	defer closeJournal()

	// The runtime is a broker over attached nodes, or nothing at all. Either
	// way this binary carries no agent machinery — nodehost reaches the
	// protocol, the link and the remote client, none of which can run a model —
	// and that is the property CI enforces. Everything else is wired exactly as
	// the combined binary wires it for ModeGateway.
	agents := app.Agents{}
	var nodeEndpoint app.NodeEndpoint
	if addr := strings.TrimSpace(*nodeListen); addr != "" {
		tokens, err := configstore.OpenNodeTokens(ctx, cfg.Database, cfg.BaseDir, cfg.BaseName)
		if err != nil {
			return fmt.Errorf("open the node credential store: %w", err)
		}
		// The journal is handed over because #170 says node disconnects are
		// journalled and not announced: a laptop sleeping at six o'clock
		// disconnects every evening, and a DM about it trains the admin to
		// ignore the one that matters. The binary already opened it above; the
		// Host had no way to reach it before #195.
		// Where a delegation is written down. It follows database.backend for
		// the same reason the credential store does: whichever gateway is
		// leading has to read the pin the previous leader wrote, or a failover
		// silently moves every live conversation onto a different machine.
		pins, err := configstore.OpenConversationPins(ctx, cfg.Database, cfg.BaseDir, cfg.BaseName)
		if err != nil {
			return fmt.Errorf("open the conversation pin store: %w", err)
		}
		defer func() { _ = pins.Close() }()
		host, err := nodehost.New(nodehost.Options{
			Tokens: tokens, Logger: logger, Journal: recorder, Pins: pins,
			Advertise: strings.TrimSpace(*nodeAdvertise),
		})
		if err != nil {
			return err
		}
		nodeRuntime := nodehost.Runtime(host)
		agents.Runtime = func(cfg config.Config, _ *tools.Registry, logger *slog.Logger) agentruntime.Builder {
			return nodeRuntime(cfg, logger)
		}
		// Bound at process start, accepting only while elected. #170 says only
		// the elected gateway accepts nodes, and a listener that came and went
		// with leadership would have to re-acquire its port at exactly the
		// moment a failover is already going badly. So the port is held and the
		// accept is gated — by the election, wired below, without which this
		// endpoint refuses everything.
		nodeEndpoint = app.NodeEndpoint{
			Address: host.Address,
			Follow:  func(v app.LeaderView) { host.FollowLeader(v) },
			Detach:  host.DetachAll,
			// #170 Change I's onboarding trigger, and the connect-time check
			// that replaced a write-time one. Both are answers only the Slack
			// side has and only the node registry needs, so the binary holding
			// both ends hands them over — the same shape as Follow above.
			Onboard: func(o app.NodeOnboarding) {
				host.WithOnboarding(o.References, func(ctx context.Context, node nodehost.Node) {
					o.Unconfigured(ctx, node.NodeID, node.UserID)
				}, func(node nodehost.Node) {
					o.Settled(node.NodeID, node.UserID)
				})
			},
			Configure: host.Configure,
		}
		go func() {
			if err := host.Listen(ctx, addr); err != nil {
				// The gateway keeps serving Slack: a broken listener means no
				// node can attach, which chat already reports as having no
				// agent. Taking Slack down as well would turn a degraded
				// gateway into a silent one.
				logger.Error("runtime node endpoint stopped", "error", err, "addr", addr)
			}
		}()
	}

	application := app.New(app.ModeGateway, nil, cfg, cfgStore, path, version, logger, recorder, agents).
		// stop is reused as the restart coordinator's cancel hook so a
		// user-triggered restart looks identical to a SIGTERM from the outside
		// (launchd, systemd): the process exits 0 and the supervisor respawns it.
		WithRestartCoordinator(app.NewRestartCoordinator(stop, logger, 0, 0)).
		WithNodeEndpoint(nodeEndpoint).
		WithJournalRetentionSweep(store, cfg, logger)
	if marker, err := app.DefaultResumeMarkerPath(); err != nil {
		logger.Warn("resume marker disabled: could not resolve state directory", "error", err)
	} else {
		application = application.WithResumeMarkerPath(marker)
	}

	if err := application.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// newLogger builds the daemon's logger. Unlike the combined binary there is no
// CLI mode to be quiet for, so info is the floor.
func newLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
