// Package nodeapp is the runtime binary's composition root. It owns the tool
// registry a node exposes over its CLI and its MCP stdio server. It is separate
// from internal/app because that one reaches Slack, which a node must not.
package nodeapp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentruntime/local"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/frontends/cli"
	"github.com/miere/murtaugh/internal/frontends/mcp"
	"github.com/miere/murtaugh/internal/help"
	"github.com/miere/murtaugh/internal/tools"
	"github.com/miere/murtaugh/internal/tools/ask"
	authrequest "github.com/miere/murtaugh/internal/tools/auth/request"
	cfgtools "github.com/miere/murtaugh/internal/tools/cfg"
	"github.com/miere/murtaugh/internal/tools/helptool"
	"github.com/miere/murtaugh/internal/tools/jobs/run"
	"github.com/miere/murtaugh/internal/tools/ping"
	"github.com/miere/murtaugh/internal/tools/plan"
	setupupdate "github.com/miere/murtaugh/internal/tools/setup/update"
	versiontool "github.com/miere/murtaugh/internal/tools/version"
)

// Program is the binary name used in the CLI's error messages.
const Program = "murtaugh-runtime"

// Registry wires every tool the runtime binary offers over its CLI and its MCP
// stdio server. cfgStore may be nil: the cfg tools then fail cleanly at invoke
// time rather than at construction, which is what lets `help` work on a machine
// that has never been configured.
func Registry(cfg config.Config, cfgStore config.Store, configPath, version string) *tools.Registry {
	reg := tools.NewRegistry()
	reg.Register(ping.New())
	reg.Register(versiontool.New(version))
	reg.Register(helptool.New(func() []help.Doc { return HelpDocs(reg) }))

	// The node keeps the local agent: `jobs run` starts one in this process,
	// with no gateway and no broker in the way. When the broker is broken this
	// is the way to run an agent at all.
	jobsLookup := func(name string) (config.JobProfile, bool) {
		j, ok := cfg.Jobs[name]
		return j, ok
	}
	reg.Register(run.New(jobsLookup).WithDelegator(delegator(cfg, reg)))

	for _, t := range cfgtools.NodeTools(cfgStore, configPath, cfgtools.InstallerDeps{
		Role:       config.RoleNode,
		Home:       os.UserHomeDir,
		GOOS:       runtime.GOOS,
		Plutil:     execRunner,
		Executable: os.Executable,
		ConfigPath: configPath,
	}) {
		reg.Register(t)
	}

	// These three reach a person. Off the node's own connection to a gateway
	// there is nobody to reach, so they answer on the terminal.
	reg.Register(ask.New(agent.TurnDisplay{}))
	reg.Register(plan.New(agent.TurnDisplay{}))
	reg.Register(authrequest.New(agent.TurnDisplay{}))

	reg.Register(setupupdate.New(setupupdate.Deps{
		CurrentVersion: func() string { return version },
		HTTPGet:        setupupdate.HTTPGetter(),
		Owner:          "miere",
		Repo:           "murtaugh",
	}))
	return reg
}

// RunCLI dispatches one command against the runtime's registry.
func RunCLI(ctx context.Context, reg *tools.Registry, args []string, jsonOutput bool) error {
	return cli.New(Program, reg).WithJSON(jsonOutput).Run(ctx, args)
}

// RunMCP serves the runtime's registry over MCP on stdio. It is how an agent on
// this machine reaches Murtaugh's tools.
func RunMCP(ctx context.Context, reg *tools.Registry) error {
	return mcp.New(reg).Serve(ctx)
}

// HelpReference builds the command reference from the real registry, before any
// configuration is loaded, so `help` works on a machine that has never been
// configured.
func HelpReference(version string) *help.Reference {
	return help.New(HelpDocs(Registry(config.Config{}, nil, "", version)))
}

// HelpDocs adapts a registry to the reference's Doc view.
func HelpDocs(reg *tools.Registry) []help.Doc {
	all := reg.All()
	out := make([]help.Doc, 0, len(all))
	for _, t := range all {
		out = append(out, t)
	}
	return out
}

// A typed nil would slip past jobs.run's nil check and report every job as
// delegable on a node that holds no agent profile.
func delegator(cfg config.Config, reg *tools.Registry) run.AgentDelegator {
	d := local.Delegator(cfg, reg)
	if d == nil {
		return nil
	}
	return d
}

// execRunner runs name with args, surfacing combined output only on failure so
// a successful invocation stays quiet on the CLI.
func execRunner(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
