// Command murtaugh is the single entry point for the Murtaugh dev toolkit.
// It can run as the Slack gateway — the Socket Mode daemon started by
// `murtaugh slack gateway` — the MCP stdio server (`murtaugh mcp`), or
// invoke any of the registered CLI tools directly (e.g. `murtaugh ping`,
// `murtaugh jobs run --name X`, `murtaugh slack send-msg --to ...`).
//
// All modes share the same loaded config and the same Tool registry, so
// adding a new tool exposes it to both the CLI and MCP frontends in a
// single change.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/miere/murtaugh/internal/agentruntime/local"
	"github.com/miere/murtaugh/internal/app"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/migrate"
	configstore "github.com/miere/murtaugh/internal/config/store"
	"github.com/miere/murtaugh/internal/help"
	"github.com/miere/murtaugh/internal/journal"
	"github.com/miere/murtaugh/internal/mcpbridge"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "murtaugh:", err)
		os.Exit(1)
	}
}

// run parses the top-level flags and mode, loads the config, and delegates
// to the application layer. It is separated from main() so it can be
// exercised by tests.
func run(rawArgs []string) error {
	if len(rawArgs) > 0 && rawArgs[0] == "version" {
		fmt.Println(version)
		return nil
	}
	// `murtaugh mcp-bridge` is the transparent stdio↔socket proxy the gateway
	// hands an ACP agent to reach Murtaugh's per-agent MCP aggregator. It is
	// spawned by the agent (not a user), needs no config, and must keep stdout
	// clean for MCP — so it is dispatched here, before any config bootstrap or
	// logging is wired.
	if len(rawArgs) > 0 && rawArgs[0] == mcpbridge.Subcommand {
		return runMCPBridge()
	}
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}

	configPath, args, err := extractConfigFlag(rawArgs, defaultPath)
	if err != nil {
		return err
	}
	// `config migrate` runs the schema migration on demand (the same path the
	// daemon runs automatically at startup), so an operator can convert a config
	// dir without launching the gateway.
	if len(args) >= 2 && args[0] == "config" && args[1] == "migrate" {
		return runConfigMigrate(filepath.Dir(configPath))
	}
	// --json is a global, opt-in boolean stripped before help/mode selection
	// and tool dispatch. The tool flag parser requires every --flag to carry a
	// value, so a bare --json must not reach it; stripping here lets both
	// `murtaugh --json ping` and `murtaugh ping --json` work.
	jsonOutput, args, err := extractJSONFlag(args)
	if err != nil {
		return err
	}
	// Help is resolved before config bootstrap/load so `murtaugh help` (and
	// `murtaugh <command> --help`) work on a machine that has never been
	// configured. The single embedded reference is the source of truth.
	if tokens, ok := helpRequest(args); ok {
		fmt.Fprint(os.Stdout, help.Render(tokens))
		return nil
	}
	mode, rest := selectMode(args)
	setupInvocation := isSetupInvocation(mode, rest)

	// Convert a legacy config directory to the current schema before bootstrap
	// seeds a fresh template. Each step is backup/validate/rollback-guarded, so a
	// failure here leaves the original config intact. Skipped for setup tools:
	// they are actively constructing the config (it may be partial / token-less),
	// which is not a state to migrate or validate.
	if !setupInvocation {
		if applied, err := migrate.Run(filepath.Dir(configPath)); err != nil {
			return fmt.Errorf("config migration failed: %w", err)
		} else if len(applied) > 0 {
			fmt.Fprintf(os.Stderr, "murtaugh: migrated config to schema v%d\n", applied[len(applied)-1])
		}
	}

	// The bootstrap file was renamed gateway.yaml → config.yaml. Adopt an existing
	// gateway.yaml — from an older install, or just produced by the schema
	// migration above — so upgrades are seamless before bootstrap seeds a fresh one.
	if err := adoptLegacyBootstrap(configPath); err != nil {
		return fmt.Errorf("adopt legacy config file: %w", err)
	}

	// Seeded for the role this invocation addresses, not always for a gateway.
	// This call runs BEFORE anything reads an argument, so pointing --config at
	// a node's root used to create a config.yaml advertising ${SLACK_APP_TOKEN}
	// there — the exact file item 12 removed from a node — and the later
	// `--role node` seeding would then preserve it, because bootstrap never
	// overwrites a config.yaml that already exists.
	if err := config.BootstrapRole(configPath, roleFor(mode, rest)); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Resolve config from the store. On an upgrade this migrates the legacy YAML
	// tree into a fresh SQLite database (rewriting config.yaml down to oauth +
	// database, archiving the siblings). Setup tools open the store but skip the
	// Load/validate — they run before a valid config exists and only need the
	// store handle to write into.
	cfg, cfgStore, err := configstore.BootstrapRole(ctx, configPath, roleFor(mode, rest), setupInvocation)
	if err != nil {
		return err
	}
	defer cfgStore.Close()

	logger := newLogger(cfg.Access.Debug, mode)

	// The journal records agent-facing domain events (gateway interactions, job
	// runs). It is opened here so its drain-on-shutdown is tied to process exit;
	// a failure to open degrades to a no-op recorder rather than blocking start.
	store, recorder, closeJournal := openJournal(cfg, mode, rest, logger)
	defer closeJournal()

	// The CLI keeps its local agent, and so does this binary's gateway mode: it
	// is the one that serves today, and #170 Stage 1 removes nothing from it.
	// Naming the implementation here rather than inside internal/app is what
	// makes "which binaries can run an agent" answerable by reading the imports
	// of a main package — cmd/murtaugh-gateway names none, and CI proves it.
	agents := app.Agents{Runtime: local.Builder, Delegator: local.Delegator}
	application := app.New(mode, rest, cfg, cfgStore, configPath, version, logger, recorder, agents).
		WithJSONOutput(jsonOutput)
	// The Slack gateway is the only long-running mode that needs a
	// user-triggered restart path. stop is reused as the cancel hook so
	// the coordinator's shutdown looks identical to a SIGTERM from the
	// outside (launchd, systemd) — process exits 0, supervisor respawns.
	if mode == app.ModeGateway {
		application = application.WithRestartCoordinator(
			app.NewRestartCoordinator(stop, logger, 0, 0),
		)
		if path, err := app.DefaultResumeMarkerPath(); err != nil {
			logger.Warn("resume marker disabled: could not resolve state directory", "error", err)
		} else {
			application = application.WithResumeMarkerPath(path)
		}
		application = application.WithJournalRetentionSweep(store, cfg, logger)
	}
	if mode == app.ModeCLI && len(rest) == 0 {
		return errors.New(application.UsageLine())
	}
	// A bare `murtaugh slack` (no subcommand) lists the slack subcommands
	// instead of trying to resolve a tool literally named "slack".
	if mode == app.ModeCLI && len(rest) == 1 && rest[0] == "slack" {
		return errors.New(application.SlackUsageLine())
	}
	return application.Run(ctx)
}

// runMCPBridge runs the `murtaugh mcp-bridge` subcommand: a transparent pipe
// between the spawning agent's stdio and the gateway's aggregator socket. The
// socket path and session token arrive via the environment so no argument
// parsing (which could collide with config flags) is needed. It blocks until the
// agent closes the pipe or the process is signalled.
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

// extractConfigFlag pulls the global --config flag out of args, supporting
// both `--config=VALUE` and `--config VALUE` (and the single-dash variants).
// Unknown flags are passed through to the selected frontend untouched.
//
// It scans the WHOLE command line, before and after the subcommand, because
// `murtaugh --config X ping` and `murtaugh ping --config X` are both documented.
// The cost of that reach is that no tool may ever take a flag called `config`:
// its value would be eaten here and the tool would see nothing. That is not
// hypothetical — `cfg node split --config <dest>` shipped that way and ran the
// entire command against the destination — so a SECOND `--config` is now an
// error rather than last-one-wins. Retargeting the whole invocation is never
// what somebody who typed it twice meant, and the alternative to saying so is
// the silent version of it.
func extractConfigFlag(args []string, fallback string) (string, []string, error) {
	out := make([]string, 0, len(args))
	configPath := fallback
	seen := false
	set := func(value string) error {
		if seen {
			return fmt.Errorf("--config given twice (%q then %q): it is a GLOBAL flag naming the configuration this command runs against, and applies to the whole command line. "+
				"A destination path belongs to the subcommand's own flag, e.g. `cfg node split --dest`", configPath, value)
		}
		seen, configPath = true, value
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, value, hasValue := parseConfigToken(a)
		if name != "config" {
			out = append(out, a)
			continue
		}
		if hasValue {
			if err := set(value); err != nil {
				return "", nil, err
			}
			continue
		}
		if i+1 >= len(args) {
			return "", nil, errors.New("--config requires a value")
		}
		if err := set(args[i+1]); err != nil {
			return "", nil, err
		}
		i++
	}
	return configPath, out, nil
}

// parseConfigToken inspects a token for the --config / -config flag form.
// It returns the bare flag name (without dashes), the embedded value when
// the token uses the --key=value form, and whether such a value was present.
// Tokens that do not look like the config flag return name="".
func parseConfigToken(a string) (string, string, bool) {
	if !strings.HasPrefix(a, "-") {
		return "", "", false
	}
	trimmed := strings.TrimLeft(a, "-")
	name, value, hasValue := trimmed, "", false
	if i := strings.IndexByte(trimmed, '='); i >= 0 {
		name = trimmed[:i]
		value = trimmed[i+1:]
		hasValue = true
	}
	return name, value, hasValue
}

// extractJSONFlag pulls the global --json boolean out of args and returns
// whether it was set along with the remaining tokens. A bare `--json` enables
// it; the `--json=true` / `--json=false` form is also honoured. It is stripped
// before help/mode selection and tool dispatch because the tool flag parser
// rejects value-less flags. Single-dash `-json` is accepted to match the
// config flag's leniency.
func extractJSONFlag(args []string) (bool, []string, error) {
	out := make([]string, 0, len(args))
	enabled := false
	for _, a := range args {
		name, value, hasValue := parseConfigToken(a)
		if name != "json" {
			out = append(out, a)
			continue
		}
		if !hasValue {
			enabled = true
			continue
		}
		b, err := strconv.ParseBool(value)
		if err != nil {
			return false, nil, fmt.Errorf("--json: expected boolean, got %q", value)
		}
		enabled = b
	}
	return enabled, out, nil
}

// helpRequest reports whether args asks for help and, if so, returns the
// command tokens that scope it (empty means the full document). A leading
// `help` subcommand consumes the rest of the tokens as the command to look up
// (`murtaugh help slack send-msg`); otherwise a `--help`/`-h` flag anywhere
// triggers help scoped to the surrounding command (`murtaugh slack send-msg
// --help`). The `--config` flag has already been stripped by the caller.
func helpRequest(args []string) ([]string, bool) {
	if len(args) > 0 && args[0] == "help" {
		return args[1:], true
	}
	tokens := make([]string, 0, len(args))
	found := false
	for _, a := range args {
		if a == "--help" || a == "-h" {
			found = true
			continue
		}
		tokens = append(tokens, a)
	}
	if found {
		return tokens, true
	}
	return nil, false
}

// openJournal opens the event journal for this invocation. Setup tools run
// before a valid config exists, so they get a nil store and a no-op recorder;
// everything else takes the daemon's own opener, which degrades the same way
// when the store cannot be opened. The caller must invoke the returned cleanup
// before exit so buffered events flush.
func openJournal(cfg config.Config, mode app.Mode, rest []string, logger *slog.Logger) (*journal.Store, journal.Recorder, func()) {
	if isSetupInvocation(mode, rest) {
		return nil, journal.NopRecorder{}, func() {}
	}
	return app.OpenJournal(cfg, logger)
}

// adoptLegacyBootstrap renames a legacy gateway.yaml to configPath when the
// target does not yet exist. It bridges the gateway.yaml → config.yaml rename
// for existing installs (and for the output of the schema migration), so an
// upgrade is seamless. It is a no-op when configPath already exists or when the
// operator pointed --config at a gateway.yaml directly.
func adoptLegacyBootstrap(configPath string) error {
	if filepath.Base(configPath) == "gateway.yaml" {
		return nil // operator explicitly targeted the legacy file
	}
	if _, err := os.Stat(configPath); err == nil {
		return nil // config.yaml already present
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	legacy := filepath.Join(filepath.Dir(configPath), "gateway.yaml")
	if _, err := os.Stat(legacy); err != nil {
		return nil // nothing to adopt
	}
	return os.Rename(legacy, configPath)
}

// isSetupInvocation reports whether the CLI was asked to run a setup.* tool.
// Setup tools intentionally run before config.Load — they exist precisely to
// produce the file that Load would otherwise validate.
func isSetupInvocation(mode app.Mode, rest []string) bool {
	if mode != app.ModeCLI || len(rest) == 0 {
		return false
	}
	return rest[0] == "setup"
}

// roleFor decides which half of #170's split THIS invocation addresses.
//
// The role is set by the binary and never read from the file — a configuration
// that could declare itself a gateway would be asserting a role the gateway
// then trusts — so it is decided from the command line, and exactly two
// commands decide it.
//
// `cfg node set` and `cfg node show` are the documented way to point an
// installed node at its gateway: named in assets/cli-help.md, instructed inside
// assets/node-config.yaml, and offered as the remedy by murtaugh-runtime's own
// "no gateway address" error. Their subject IS the node's own root, which by
// design has no `oauth:` block — so run at RoleCombined they die on
// "oauth.app_token is required", asking an operator for a credential a node must
// never hold and which they therefore cannot supply.
//
// `cfg node split` is deliberately NOT one of them. It runs from the GATEWAY's
// root and writes the node's, which is why it already worked.
//
// RoleNode is only ever more permissive than RoleCombined here: it drops the
// Slack-credential requirement and keeps every name→body check, since a node
// holds the bodies. So pointing either of these two at a combined root still
// does exactly what it did.
func roleFor(mode app.Mode, rest []string) config.Role {
	if mode != app.ModeCLI || len(rest) < 2 {
		return config.RoleCombined
	}
	// Any `setup … --role runtime` addresses the RUNTIME NODE's root — the
	// installer's `--role runtime|both` path, which runs `setup bootstrap` and
	// `setup launchd` against it. It is the third exception and it is the same
	// exception: the subject is the node's own directory, so seeding it from the
	// gateway skeleton is the failure, not a validation difference.
	//
	// It is one rule over every setup tool rather than a list of them, because
	// the rule that would actually be broken is the one that forgets a tool: a
	// setup command run without it seeds a config.yaml advertising
	// ${SLACK_APP_TOKEN} into the node's root, before the tool it names has
	// done anything at all.
	if rest[0] == "setup" && namesRuntimeRole(rest[1:]) {
		return config.RoleNode
	}
	if len(rest) < 3 || rest[0] != "cfg" || rest[1] != "node" {
		return config.RoleCombined
	}
	switch rest[2] {
	case "set", "show":
		return config.RoleNode
	default:
		return config.RoleCombined
	}
}

// namesRuntimeRole reports whether the tool arguments carry `--role runtime`,
// in either of the two spellings the CLI's argument parser accepts.
//
// `runtime` and not `node`: it is the word the installer's --role takes, the
// word setup.launchd takes, and the name of the binary it starts. config.RoleNode
// is the internal spelling and does not have to be the operator's.
func namesRuntimeRole(args []string) bool {
	for i, arg := range args {
		switch {
		case arg == "--role" || arg == "-role":
			return i+1 < len(args) && strings.EqualFold(strings.TrimSpace(args[i+1]), "runtime")
		case strings.HasPrefix(arg, "--role="), strings.HasPrefix(arg, "-role="):
			_, value, _ := strings.Cut(arg, "=")
			return strings.EqualFold(strings.TrimSpace(value), "runtime")
		}
	}
	return false
}

// selectMode resolves the top-level subcommand. `slack gateway` starts the
// long-running Socket Mode daemon; `slack <tool>` and every other token are
// CLI tools resolved by the registry. No subcommand prints usage (ModeCLI
// with an empty arg list), so the gateway is always launched explicitly.
// runConfigMigrate converts the config directory to the current schema and
// prints what it did. It is the manual entrypoint for the same migration the
// daemon runs at startup; both are backup/validate/rollback-guarded.
func runConfigMigrate(dir string) error {
	applied, err := migrate.Run(dir)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		fmt.Fprintln(os.Stdout, "config is already at the current schema; nothing to migrate")
		return nil
	}
	fmt.Fprintf(os.Stdout, "migrated %s to schema v%d\n", dir, applied[len(applied)-1])
	return nil
}

func selectMode(args []string) (app.Mode, []string) {
	if len(args) == 0 {
		return app.ModeCLI, nil
	}
	switch args[0] {
	case "slack":
		// `slack gateway` is the daemon; `slack <tool>` falls through to
		// the CLI, where resolve() forms the dotted name "slack.<tool>".
		// A bare `slack` also falls through; run() then prints the slack
		// subcommand list.
		if len(args) >= 2 && args[1] == "gateway" {
			return app.ModeGateway, args[2:]
		}
		return app.ModeCLI, args
	case "mcp":
		return app.ModeMCP, args[1:]
	default:
		return app.ModeCLI, args
	}
}

// newLogger builds the slog logger Murtaugh uses for daemon-style modes.
// CLI invocations get a quieter logger so tool output dominates stdout/
// stderr.
func newLogger(debug bool, mode app.Mode) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	if mode == app.ModeCLI {
		level = slog.LevelWarn
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
