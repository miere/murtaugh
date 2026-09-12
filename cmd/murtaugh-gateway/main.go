// Command murtaugh-gateway is the Slack gateway without any agent code linked in.
// Keep it that way: CI's reachability check fails if an agent backend is imported.
//
// With no command it runs the daemon. With one it runs that command against the
// gateway's own configuration — the gateway admin's surface, and nothing that
// needs an agent.
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
	"github.com/miere/murtaugh/internal/cmdline"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/migrate"
	configstore "github.com/miere/murtaugh/internal/config/store"
	"github.com/miere/murtaugh/internal/nodehost"
	"github.com/miere/murtaugh/internal/tools"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, app.Program+":", err)
		os.Exit(1)
	}
}

func run(rawArgs []string) error {
	if len(rawArgs) > 0 && rawArgs[0] == "version" {
		fmt.Println(version)
		return nil
	}
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}
	configPath, args, err := cmdline.ExtractConfigFlag(rawArgs, defaultPath)
	if err != nil {
		return err
	}
	jsonOutput, args, err := cmdline.ExtractJSONFlag(args)
	if err != nil {
		return err
	}
	// Help is resolved before the configuration is touched, so it works on a
	// machine that has never been configured. The flag tables come from the
	// tools themselves, so they cannot disagree with what this binary accepts.
	if tokens, ok := cmdline.HelpRequest(args); ok {
		fmt.Fprint(os.Stdout, app.HelpReference(version).Render(tokens))
		return nil
	}

	command := cmdline.IsCommand(args)
	var nodeListen, nodeAdvertise string
	if !command {
		fs := flag.NewFlagSet(app.Program, flag.ContinueOnError)
		fs.StringVar(&nodeListen, "node-listen", "", "address to accept runtime node connections on, e.g. 127.0.0.1:8787 (empty: accept none)")
		fs.StringVar(&nodeAdvertise, "node-advertise", "", "address(es) nodes should use to reach this gateway, space- or comma-separated and used verbatim, e.g. \"wss://gateway.example.com wss://192.0.2.10:8443\" (empty: work it out from the listener, which yields ws:// and is only dialable on loopback)")
		showVersion := fs.Bool("version", false, "print the version and exit")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *showVersion {
			fmt.Println(version)
			return nil
		}
	}

	if err := prepareConfigDir(configPath); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, cfgStore, err := configstore.BootstrapRole(ctx, configPath, config.RoleGateway, false)
	if err != nil {
		return err
	}
	defer func() { _ = cfgStore.Close() }()

	if command {
		application := app.New(app.ModeCLI, args, cfg, cfgStore, configPath, version, newLogger(cfg.Access.Debug, true), nil, app.Agents{}).
			WithJSONOutput(jsonOutput)
		return application.Run(ctx)
	}
	return runDaemon(ctx, stop, cfg, cfgStore, configPath, nodeListen, nodeAdvertise)
}

// prepareConfigDir brings the configuration directory up to what this binary
// expects. Migration runs first but only over a directory that exists: a fresh
// path has nothing to migrate, and stamping a schema version into a directory
// that is not there yet used to kill the first start with a missing
// `.schema_version` instead of the missing credential.
func prepareConfigDir(configPath string) error {
	if applied, err := migrate.Run(filepath.Dir(configPath)); err != nil {
		return fmt.Errorf("config migration failed: %w", err)
	} else if len(applied) > 0 {
		fmt.Fprintf(os.Stderr, "%s: migrated config to schema v%d\n", app.Program, applied[len(applied)-1])
	}
	// The bootstrap file was renamed gateway.yaml → config.yaml. Adopt an
	// existing gateway.yaml — from an older install, or just produced by the
	// schema migration above — so an upgrade is seamless.
	if err := adoptLegacyBootstrap(configPath); err != nil {
		return fmt.Errorf("adopt legacy config file: %w", err)
	}
	return config.Bootstrap(configPath)
}

func runDaemon(ctx context.Context, stop func(), cfg config.Config, cfgStore config.Store, configPath, nodeListen, nodeAdvertise string) error {
	logger := newLogger(cfg.Access.Debug, false)
	store, recorder, closeJournal := app.OpenJournal(cfg, logger)
	defer closeJournal()

	agents := app.Agents{}
	var nodeEndpoint app.NodeEndpoint
	if addr := strings.TrimSpace(nodeListen); addr != "" {
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
			Advertise: strings.TrimSpace(nodeAdvertise),
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

	application := app.New(app.ModeGateway, nil, cfg, cfgStore, configPath, version, logger, recorder, agents).
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

// adoptLegacyBootstrap renames a legacy gateway.yaml to configPath when the
// target does not yet exist. It is a no-op when configPath already exists or
// when the operator pointed --config at a gateway.yaml directly.
func adoptLegacyBootstrap(configPath string) error {
	if filepath.Base(configPath) == "gateway.yaml" {
		return nil
	}
	if _, err := os.Stat(configPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	legacy := filepath.Join(filepath.Dir(configPath), "gateway.yaml")
	if _, err := os.Stat(legacy); err != nil {
		return nil
	}
	return os.Rename(legacy, configPath)
}

// newLogger keeps a one-shot command quiet so its output dominates the
// terminal, while the daemon logs at the configured level.
func newLogger(debug, quiet bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	if quiet {
		level = slog.LevelWarn
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
