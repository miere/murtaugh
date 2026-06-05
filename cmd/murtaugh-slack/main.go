package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/miere/murtaugh-dev-toolkit/internal/slackapp"
)

func main() {
	defaultConfigPath, err := config.DefaultPath()
	if err != nil {
		fatal(err)
	}

	configPath := flag.String("config", defaultConfigPath, "path to Slack configuration YAML")
	flag.Parse()

	if err := config.Bootstrap(*configPath); err != nil {
		fatal(err)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal(err)
	}

	logger := newLogger(cfg.Slack.Debug)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app := slackapp.New(cfg, logger)
	logger.Info("starting Slack Socket Mode service", "config", *configPath)
	if err := app.Run(ctx); err != nil && ctx.Err() == nil {
		fatal(err)
	}
	logger.Info("Slack Socket Mode service stopped")
}

func newLogger(debug bool) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
