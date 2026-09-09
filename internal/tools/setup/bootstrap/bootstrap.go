// Package bootstrap implements the `setup.bootstrap` tool: seed the config
// directory with the embedded defaults (config.yaml, agents.yaml, jobs.yaml,
// skills/, optional docs) the first time Murtaugh is installed.
//
// The tool wraps config.BootstrapWithReport so the installer (and any MCP
// client driving setup remotely) can report which files were freshly written
// versus which were preserved because the user already customised them.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/config"
)

// PathProvider returns the path of the primary config file (config.yaml). The
// config directory is derived from filepath.Dir of that value, matching the
// convention used by the rest of the codebase.
type PathProvider func() string

// Tool is the `setup.bootstrap` capability.
type Tool struct {
	path PathProvider
}

// New constructs a Tool that seeds the directory containing the file path
// returned by path.
func New(path PathProvider) *Tool {
	return &Tool{path: path}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "setup.bootstrap" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Seed the Murtaugh config directory with embedded defaults (idempotent)."
}

// InputSchema documents the optional force flag.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"force": {
				Type:        "boolean",
				Description: "Refresh the bundled default system prompt to the shipped version (user config, secrets, and AGENTS.md are always preserved).",
			},
			"role": {
				Type:        "string",
				Enum:        []any{"gateway", "runtime"},
				Description: "Which root to seed: gateway (the default) or runtime. A runtime node's root gets a config.yaml with no oauth block and an .env template naming no Slack variable.",
			},
		},
	}
}

// Result is the structured payload returned by Invoke.
type Result struct {
	ConfigDir string   `json:"config_dir"`
	Created   []string `json:"created"`
	Updated   []string `json:"updated"`
	Preserved []string `json:"preserved"`
}

// String renders a multi-line CLI confirmation summarising the report.
func (r Result) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "bootstrap: %d created, %d updated, %d preserved in %s",
		len(r.Created), len(r.Updated), len(r.Preserved), r.ConfigDir)
	for _, p := range r.Created {
		fmt.Fprintf(&b, "\n  + %s", p)
	}
	for _, p := range r.Updated {
		fmt.Fprintf(&b, "\n  ~ %s", p)
	}
	for _, p := range r.Preserved {
		fmt.Fprintf(&b, "\n  = %s", p)
	}
	return b.String()
}

// Invoke seeds the config directory and returns a structured report of the
// result. The optional force flag refreshes the bundled default system prompt.
func (t *Tool) Invoke(_ context.Context, args map[string]any) (any, error) {
	path := t.path()
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("config path is not configured")
	}
	force, _ := args["force"].(bool)

	// The node asset is not a variation on the gateway's, it is the ABSENCE of
	// something: node-config.yaml has no `oauth:` block and node-env.example
	// names no SLACK_* variable. Seeding a node root from the gateway skeleton
	// would put a file advertising ${SLACK_APP_TOKEN} on every laptop in the
	// fleet — an invitation to fill it in, on the one machine #170 is explicit
	// must never hold those tokens.
	report, err := bootstrapFor(role(args), path, force)
	if err != nil {
		return nil, err
	}
	return Result{
		ConfigDir: filepath.Dir(path),
		Created:   report.Created,
		Updated:   report.Updated,
		Preserved: report.Preserved,
	}, nil
}

// role reads the role argument. An unknown value is not defaulted away: the two
// roots differ by a credential block, and silently seeding the wrong one is not
// a thing to recover from by guessing.
func role(args map[string]any) string {
	v, _ := args["role"].(string)
	return strings.ToLower(strings.TrimSpace(v))
}

func bootstrapFor(role, path string, force bool) (config.BootstrapReport, error) {
	switch role {
	case "", "gateway":
		return config.BootstrapWithReport(path, force)
	case "runtime":
		return config.BootstrapNodeWithReport(path, force)
	default:
		return config.BootstrapReport{}, fmt.Errorf("unknown role %q: expected \"gateway\" or \"runtime\"", role)
	}
}
