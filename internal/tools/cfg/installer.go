package cfg

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/migrate"
	"github.com/miere/murtaugh/internal/launchagent"
	"github.com/miere/murtaugh/internal/tools"
)

// CommandRunner invokes name with args. Only plutil is run this way, so a test
// can lint nothing without shelling out.
type CommandRunner func(ctx context.Context, name string, args ...string) error

// InstallerDeps is what a binary must tell the cfg tools about itself before
// they can configure it: which role it plays, where it lives, and which
// configuration it was started against.
type InstallerDeps struct {
	Role       config.Role
	Home       func() (string, error)
	GOOS       string
	Plutil     CommandRunner
	Executable func() (string, error)
	ConfigPath string
	// Overridden in tests so a plist never lands in the operator's real
	// ~/Library/LaunchAgents.
	LaunchAgentsDir string
}

// InstallerTools returns the two tools every binary carries to set itself up.
// `cfg validate` is role-implied and lives with the other admin tools.
func InstallerTools(deps InstallerDeps) []tools.Tool {
	return []tools.Tool{&launchdTool{deps: deps}, &migrateTool{dir: filepath.Dir(deps.ConfigPath)}}
}

type launchdTool struct{ deps InstallerDeps }

func (t *launchdTool) Name() string { return "cfg.launchd" }
func (t *launchdTool) Description() string {
	return "Write this binary's macOS LaunchAgent so launchd runs it. Writes the plist only; loading it is `launchctl bootstrap`."
}

func (t *launchdTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"binary_path":     {Type: "string", Description: "Absolute path to the binary launchd should run. Defaults to this running binary, which is almost always what you want — put the file where you want it first."},
			"alias":           {Type: "string", Description: "Names this LaunchAgent, giving the label murtaugh.gateway.<alias> or murtaugh.node.<alias>. Defaults to `default`. Two nodes on one machine differ only by this and by the global --config."},
			"update_existing": {Type: "boolean", Description: "Replace a plist that already exists. Without it an existing plist is refused, because it may be running a live daemon."},
			"gateway":         {Type: "string", Description: "Node binary only: the gateway seed address (ws:// or wss://) written into the LaunchAgent's arguments, overriding node.gateway. Omit to use the address in the node's configuration."},
			"node_listen":     {Type: "string", Description: "Gateway binary only: the address to accept runtime node connections on, e.g. 127.0.0.1:8787. Omit to accept none."},
			"node_advertise":  {Type: "string", Description: "Gateway binary only: the address(es) nodes should use to reach this gateway, used verbatim. Omit to derive them from the listener."},
		},
	}
}

func (t *launchdTool) Invoke(_ context.Context, args map[string]any) (any, error) {
	if t.deps.GOOS != "darwin" {
		return nil, fmt.Errorf("cfg launchd is macOS only; this is %s. Run the binary under whatever supervisor this system uses", t.deps.GOOS)
	}
	arguments, err := t.arguments(args)
	if err != nil {
		return nil, err
	}
	binary := strings.TrimSpace(mustString(args, "binary_path"))
	if binary == "" {
		if t.deps.Executable == nil {
			return nil, fmt.Errorf("--binary-path is required: this binary's own path could not be resolved")
		}
		if binary, err = t.deps.Executable(); err != nil {
			return nil, fmt.Errorf("resolve this binary's path: %w", err)
		}
	}
	home, err := t.deps.Home()
	if err != nil {
		return nil, fmt.Errorf("resolve home: %w", err)
	}
	update, _ := boolArg(args, "update_existing")

	result, err := launchagent.Write(launchagent.Spec{
		Role:            t.deps.Role,
		Alias:           mustString(args, "alias"),
		BinaryPath:      binary,
		Arguments:       arguments,
		Home:            home,
		LaunchAgentsDir: t.deps.LaunchAgentsDir,
	}, update)
	if err != nil {
		return nil, err
	}
	if t.deps.Plutil != nil {
		if err := t.deps.Plutil(context.Background(), "plutil", "-lint", result.Path); err != nil {
			return nil, fmt.Errorf("plutil -lint failed: %w", err)
		}
	}
	return result, nil
}

// arguments assembles the ProgramArguments after the binary, refusing a flag
// that belongs to the other role rather than writing a plist that would fail at
// launch.
//
// The configuration the LaunchAgent runs against is the one this command was
// given: `--config` is a GLOBAL flag stripped before any tool sees it, so a
// second one here could never arrive. Write a second node's LaunchAgent by
// invoking the binary against that node's configuration.
func (t *launchdTool) arguments(args map[string]any) ([]string, error) {
	out := []string{}
	if configPath := strings.TrimSpace(t.deps.ConfigPath); configPath != "" {
		out = append(out, "--config", configPath)
	}

	gateway := strings.TrimSpace(mustString(args, "gateway"))
	listen := strings.TrimSpace(mustString(args, "node_listen"))
	advertise := strings.TrimSpace(mustString(args, "node_advertise"))

	switch t.deps.Role {
	case config.RoleNode:
		if listen != "" || advertise != "" {
			return nil, fmt.Errorf("--node-listen and --node-advertise are gateway flags; this is the node binary")
		}
		if gateway != "" {
			seed := config.NodeConfig{Gateway: []string{gateway}}
			if err := seed.Validate(); err != nil {
				return nil, err
			}
			out = append(out, "-gateway", gateway)
		}
	case config.RoleGateway:
		if gateway != "" {
			return nil, fmt.Errorf("--gateway names the gateway a node dials; this is the gateway binary")
		}
		if listen != "" {
			out = append(out, "-node-listen", listen)
		}
		if advertise != "" {
			out = append(out, "-node-advertise", advertise)
		}
	default:
		return nil, fmt.Errorf("no LaunchAgent for role %q", t.deps.Role)
	}
	return out, nil
}

// migrateTool converts the configuration directory to the schema this binary
// expects. The daemons run the same pass at startup.
type migrateTool struct{ dir string }

func (t *migrateTool) Name() string { return "cfg.migrate" }
func (t *migrateTool) Description() string {
	return "Migrate this binary's configuration directory to the schema this version expects."
}
func (t *migrateTool) InputSchema() *jsonschema.Schema { return nil }

func (t *migrateTool) Invoke(_ context.Context, _ map[string]any) (any, error) {
	applied, err := migrate.Run(t.dir)
	if err != nil {
		return nil, err
	}
	if len(applied) == 0 {
		return okResult{Message: "configuration is already at the current schema; nothing to migrate"}, nil
	}
	return okResult{Message: fmt.Sprintf("migrated %s to schema v%d", t.dir, applied[len(applied)-1])}, nil
}
