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
// That makes it, today, a gateway that answers Slack but has no agent to give a
// conversation to: chat reports no agent, and delegate-to-agent surfaces report
// that delegation is unavailable. It is built this way on purpose. The broker
// that hands conversations to runtime nodes is #170 Change G, and until it
// lands nothing selects this binary — `murtaugh slack gateway` keeps serving,
// unchanged. Building the clean one alongside means the rule guards it from
// birth, and nothing has to be un-wired later under pressure.
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
	"syscall"

	"github.com/miere/murtaugh/internal/app"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/migrate"
	configstore "github.com/miere/murtaugh/internal/config/store"
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

	cfg, cfgStore, err := configstore.Bootstrap(ctx, path, false)
	if err != nil {
		return err
	}
	defer func() { _ = cfgStore.Close() }()

	logger := newLogger(cfg.Access.Debug)
	store, recorder, closeJournal := app.OpenJournal(cfg, logger)
	defer closeJournal()

	// app.Agents is left zero: this binary carries no agent machinery, and that
	// is the property CI enforces. Everything else is wired exactly as the
	// combined binary wires it for ModeGateway.
	application := app.New(app.ModeGateway, nil, cfg, cfgStore, path, version, logger, recorder, app.Agents{}).
		// stop is reused as the restart coordinator's cancel hook so a
		// user-triggered restart looks identical to a SIGTERM from the outside
		// (launchd, systemd): the process exits 0 and the supervisor respawns it.
		WithRestartCoordinator(app.NewRestartCoordinator(stop, logger, 0, 0)).
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
