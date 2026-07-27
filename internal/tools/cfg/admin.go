package cfg

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/tools"
)

// showTool (cfg.show) dumps the entire stored configuration as indented JSON.
// It reads raw bodies from a Snapshot so it needs no credentials and never
// prints any (the oauth block lives only in gateway.yaml/.env).
type dumpTool struct{ p Provider }

func (t *dumpTool) Name() string                    { return "cfg.show" }
func (t *dumpTool) Description() string             { return "Dump the entire stored configuration as JSON." }
func (t *dumpTool) InputSchema() *jsonschema.Schema { return nil }
func (t *dumpTool) Invoke(ctx context.Context, _ map[string]any) (any, error) {
	s, err := t.p()
	if err != nil {
		return nil, err
	}
	snap, err := s.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, item := range snap.Items {
		sec, _ := out[item.Section].(map[string]json.RawMessage)
		if sec == nil {
			sec = map[string]json.RawMessage{}
			out[item.Section] = sec
		}
		sec[item.Name] = item.Body
	}
	for _, sg := range snap.Singletons {
		out[sg.Key] = sg.Body
	}
	return dumpResult{Backend: s.Backend(), Body: out}, nil
}

type dumpResult struct {
	Backend string         `json:"backend"`
	Body    map[string]any `json:"config"`
}

func (r dumpResult) String() string {
	b, err := json.MarshalIndent(r.Body, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", r.Body)
	}
	return fmt.Sprintf("# config store: %s\n%s", r.Backend, b)
}

// validateTool (cfg.validate) loads and validates the whole config.
type validateTool struct{ p Provider }

func (t *validateTool) Name() string                    { return "cfg.validate" }
func (t *validateTool) Description() string             { return "Validate the stored configuration." }
func (t *validateTool) InputSchema() *jsonschema.Schema { return nil }
func (t *validateTool) Invoke(ctx context.Context, _ map[string]any) (any, error) {
	s, err := t.p()
	if err != nil {
		return nil, err
	}
	if err := validateStore(ctx, s); err != nil {
		return nil, fmt.Errorf("invalid: %w", err)
	}
	return okResult{Message: "config is valid"}, nil
}

// fileSchema is the shared --file argument for export/import.
func fileSchema(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:       "object",
		Properties: map[string]*jsonschema.Schema{"file": {Type: "string", Description: desc}},
	}
}

// exportTool (cfg.export) writes a full snapshot as JSON, to --file or stdout.
type exportTool struct{ p Provider }

func (t *exportTool) Name() string { return "cfg.export" }
func (t *exportTool) Description() string {
	return "Export the whole config store as a JSON snapshot (to --file or stdout)."
}
func (t *exportTool) InputSchema() *jsonschema.Schema {
	return fileSchema("destination file; omit to print to stdout")
}
func (t *exportTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	s, err := t.p()
	if err != nil {
		return nil, err
	}
	snap, err := s.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return nil, err
	}
	if path, ok := stringArg(args, "file"); ok && strings.TrimSpace(path) != "" {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, fmt.Errorf("write %q: %w", path, err)
		}
		return okResult{Message: fmt.Sprintf("exported %d items + %d singletons to %s", len(snap.Items), len(snap.Singletons), path)}, nil
	}
	return rawResult(data), nil
}

// importTool (cfg.import) restores a JSON snapshot into the store, then
// validates (rolling back is not attempted — import is an operator action into
// a fresh/known store; a validation failure is reported for them to fix).
type importTool struct{ p Provider }

func (t *importTool) Name() string { return "cfg.import" }
func (t *importTool) Description() string {
	return "Import a JSON config snapshot (from --file) into the store."
}
func (t *importTool) InputSchema() *jsonschema.Schema {
	return fileSchema("snapshot file produced by `cfg export`")
}
func (t *importTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	path, err := requireString(args, "file")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	var snap config.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("parse snapshot: %w", err)
	}
	s, err := t.p()
	if err != nil {
		return nil, err
	}
	if err := s.Restore(ctx, snap); err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("imported %d items + %d singletons", len(snap.Items), len(snap.Singletons))
	if verr := validateStore(ctx, s); verr != nil {
		return nil, fmt.Errorf("%s, but the result is invalid: %w", msg, verr)
	}
	return okResult{Message: msg + "; config is valid"}, nil
}

// rawResult prints already-rendered bytes verbatim.
type rawResult []byte

func (r rawResult) String() string { return string(r) }

// AdminTools returns the store-wide admin tools.
func AdminTools(p Provider) []tools.Tool {
	return []tools.Tool{&dumpTool{p: p}, &validateTool{p: p}, &exportTool{p: p}, &importTool{p: p}}
}
