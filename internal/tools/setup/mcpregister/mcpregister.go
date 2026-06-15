// Package mcpregister implements the `setup.mcp-register` tool: register
// Murtaugh as an MCP server in a downstream client's config file. Three
// clients are supported as first-class targets:
//
//   - opencode: ~/.config/opencode/opencode.json (JSON merge)
//   - auggie:   ~/.augment/settings.json         (JSON merge)
//   - goose:    ~/.config/goose/config.yaml      (YAML merge)
//
// Each writer reads the existing file (when present), merges Murtaugh into the
// client-specific extensions block while preserving every other key verbatim,
// and writes the file back through the shared backup helper.
package mcpregister

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// HomeResolver returns the user home directory. The composition root supplies
// os.UserHomeDir; tests inject a temp directory.
type HomeResolver func() (string, error)

// Tool is the `setup.mcp-register` capability.
type Tool struct {
	home HomeResolver
	// troubleshootPath returns Murtaugh's machine-managed troubleshoot.yaml.
	// When set, registering a client that is also a known diagnostics provider
	// records it there so troubleshoot bundles auto-include its files. nil
	// disables the side effect (e.g. in unit tests of the writers).
	troubleshootPath func() string
	// knownProviders is the set of client names that are also diagnostics
	// providers (e.g. "goose"); only these are recorded in troubleshoot.yaml.
	knownProviders []string
}

// New constructs a Tool that resolves client config paths under the home
// directory returned by home. troubleshootPath/knownProviders wire the
// troubleshoot.yaml auto-include side effect; pass nil/empty to disable it.
func New(home HomeResolver, troubleshootPath func() string, knownProviders []string) *Tool {
	return &Tool{home: home, troubleshootPath: troubleshootPath, knownProviders: knownProviders}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "setup.mcp-register" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Register Murtaugh as an MCP server in opencode, auggie, or goose."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"client":      {Type: "string", Enum: []any{"opencode", "auggie", "goose"}, Description: "Downstream MCP client to configure."},
			"binary_path": {Type: "string", Description: "Absolute path to the murtaugh binary used as the MCP command."},
		},
		Required: []string{"client", "binary_path"},
	}
}

// Result is the structured payload returned by Invoke.
type Result struct {
	Client     string `json:"client"`
	Path       string `json:"path"`
	BackupPath string `json:"backup_path,omitempty"`
	Created    bool   `json:"created"`
	// ProviderRecorded is true when the client was also added to
	// troubleshoot.yaml's providers list (so bundles auto-include its files).
	ProviderRecorded bool `json:"provider_recorded,omitempty"`
	// Warning carries a non-fatal note (e.g. the client registered fine but the
	// troubleshoot.yaml update failed).
	Warning string `json:"warning,omitempty"`
}

// String renders a one-line CLI confirmation.
func (r Result) String() string {
	verb := "updated"
	if r.Created {
		verb = "created"
	}
	var b strings.Builder
	if r.BackupPath != "" {
		fmt.Fprintf(&b, "%s %s for %s (backup: %s)", verb, r.Path, r.Client, r.BackupPath)
	} else {
		fmt.Fprintf(&b, "%s %s for %s", verb, r.Path, r.Client)
	}
	if r.ProviderRecorded {
		fmt.Fprintf(&b, "; recorded %q for troubleshoot bundles", r.Client)
	}
	if r.Warning != "" {
		fmt.Fprintf(&b, "\n  ! %s", r.Warning)
	}
	return b.String()
}

// Invoke dispatches to the per-client writer.
func (t *Tool) Invoke(_ context.Context, args map[string]any) (any, error) {
	client, _ := args["client"].(string)
	binary, _ := args["binary_path"].(string)
	if strings.TrimSpace(binary) == "" {
		return nil, errors.New("binary_path is required")
	}
	home, err := t.home()
	if err != nil {
		return nil, fmt.Errorf("resolve home: %w", err)
	}
	var res Result
	switch client {
	case "opencode":
		res, err = writeOpencode(home, binary)
	case "auggie":
		res, err = writeAuggie(home, binary)
	case "goose":
		res, err = writeGoose(home, binary)
	default:
		return nil, fmt.Errorf("unsupported client %q (want opencode, auggie, or goose)", client)
	}
	if err != nil {
		return nil, err
	}
	// If the client is also a provider whose on-disk diagnostics we know how to
	// collect, record it so troubleshoot bundles include it by default. This is
	// best-effort: the client registration already succeeded, so a failure here
	// only attaches a warning rather than failing the whole call.
	if t.isKnownProvider(client) && t.troubleshootPath != nil {
		if path := strings.TrimSpace(t.troubleshootPath()); path != "" {
			recorded, recErr := recordTroubleshootProvider(path, client)
			switch {
			case recErr != nil:
				res.Warning = fmt.Sprintf("registered %s, but could not record it for troubleshoot bundles: %v", client, recErr)
			case recorded:
				res.ProviderRecorded = true
			}
		}
	}
	return res, nil
}

func (t *Tool) isKnownProvider(client string) bool {
	for _, p := range t.knownProviders {
		if strings.EqualFold(p, client) {
			return true
		}
	}
	return false
}

// existed reports whether path was on disk before the write — used to
// distinguish a create from an update without consulting backup.IfExists
// twice.
func existed(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
