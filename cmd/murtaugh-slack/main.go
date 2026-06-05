package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/miere/murtaugh-dev-toolkit/internal/slackapp"
)

const usage = `Usage: murtaugh <command> [options]

Commands:
  slack              Start the Slack Socket Mode service (default)
  jobs run <name>    Run a job defined in jobs.yaml manually

Options:
  -config string     Path to Slack configuration YAML (default: ~/.config/murtaugh/slack.yaml)
`

func main() {
	defaultConfigPath, err := config.DefaultPath()
	if err != nil {
		fatal(err)
	}

	// Parse the global -config flag before inspecting sub-commands.
	fs := flag.NewFlagSet("murtaugh", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	configPath := fs.String("config", defaultConfigPath, "path to Slack configuration YAML")

	args := os.Args[1:]
	// Peel off the sub-command so flag.Parse only sees flags.
	subCmd := "slack"
	if len(args) > 0 && args[0][0] != '-' {
		subCmd = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		fatal(err)
	}

	if err := config.Bootstrap(*configPath); err != nil {
		fatal(err)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal(err)
	}

	switch subCmd {
	case "slack":
		runSlack(cfg, *configPath)
	case "jobs":
		rest := fs.Args()
		if len(rest) < 2 || rest[0] != "run" {
			fmt.Fprint(os.Stderr, "Usage: murtaugh jobs run <job-name>\n")
			os.Exit(1)
		}
		runJob(cfg, rest[1])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", subCmd, usage)
		os.Exit(1)
	}
}

func runSlack(cfg config.Config, configPath string) {
	logger := newLogger(cfg.Configuration.Debug)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app := slackapp.New(cfg, logger)
	logger.Info("starting Slack Socket Mode service", "config", configPath)
	if err := app.Run(ctx); err != nil && ctx.Err() == nil {
		fatal(err)
	}
	logger.Info("Slack Socket Mode service stopped")
}

func runJob(cfg config.Config, name string) {
	job, ok := cfg.Jobs[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "job %q not found in jobs.yaml\n", name)
		os.Exit(1)
	}

	timeout := 10 * time.Minute
	if job.Timeout != "" {
		if d, err := time.ParseDuration(job.Timeout); err == nil {
			timeout = d
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, job.Command, job.Args...)
	cmd.Dir = job.WorkDir
	cmd.Stdin = bytes.NewReader(nil)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	fmt.Fprintf(os.Stderr, "running job %q\n", name)
	if err := cmd.Run(); err != nil {
		fatal(err)
	}
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
