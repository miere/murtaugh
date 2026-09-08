// Command murtaugh-runtime is the runtime node daemon: the process that will
// hold a connection open to a gateway and run agents on this machine.
//
// It does not do that yet, and it does not pretend to. The link a node speaks
// over — the transport, the handshake, the session and tool channels — is #170
// Changes C and D; until they exist there is nothing to dial and nothing to
// answer. What this binary is today is the entry point those changes attach to,
// plus the one thing it can honestly report now: whether this machine's
// configuration describes a node that could serve, and what it would serve.
//
// Unlike cmd/murtaugh-gateway, this binary MAY reach the agent packages — it is
// the half of the split whose whole job is running a model. It happens not to
// import them yet, because there is nothing here to prompt them: agent backends
// this binary cannot yet be asked to run would be built, reported on, and never
// used.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/migrate"
	configstore "github.com/miere/murtaugh/internal/config/store"
	"github.com/miere/murtaugh/internal/nodetoken"
)

var version = "dev"

// errNoLinkYet is what this binary exits with. It is an error rather than a
// clean exit so a supervisor that is pointed at it too early reports a failure
// instead of flapping a process that silently does nothing.
var errNoLinkYet = errors.New("a runtime node cannot attach to a gateway yet: the node link is not implemented (see #170, Changes C and D)")

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "murtaugh-runtime:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("murtaugh-runtime", flag.ContinueOnError)
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
	return errNoLinkYet
}

// reportAgents prints what this node would serve, and where the credential it
// would present lives. It is the only useful answer available before the link
// exists, and it is the answer an operator wants first: "is this machine
// configured to be a node at all?".
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
