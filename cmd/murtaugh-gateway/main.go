// Command murtaugh-gateway is the Slack gateway without any agent code linked in.
// Keep it that way: CI's reachability check fails if an agent backend is imported.
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

	cfg, cfgStore, err := configstore.BootstrapRole(ctx, path, config.RoleGateway, false)
	if err != nil {
		return err
	}
	defer func() { _ = cfgStore.Close() }()

	logger := newLogger(cfg.Access.Debug)
	store, recorder, closeJournal := app.OpenJournal(cfg, logger)
	defer closeJournal()

	agents := app.Agents{}
	var nodeEndpoint app.NodeEndpoint
	if addr := strings.TrimSpace(*nodeListen); addr != "" {
		tokens, err := configstore.OpenNodeTokens(ctx, cfg.Database, cfg.BaseDir, cfg.BaseName)
		if err != nil {
			return fmt.Errorf("open the node credential store: %w", err)
		}
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
		nodeEndpoint = app.NodeEndpoint{
			Address: host.Address,
			Follow:  func(v app.LeaderView) { host.FollowLeader(v) },
			Detach:  host.DetachAll,
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
				logger.Error("runtime node endpoint stopped", "error", err, "addr", addr)
			}
		}()
	}

	application := app.New(app.ModeGateway, nil, cfg, cfgStore, path, version, logger, recorder, agents).
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

func newLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
