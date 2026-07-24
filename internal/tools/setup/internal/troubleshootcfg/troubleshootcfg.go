// Package troubleshootcfg records downstream diagnostics providers into
// Murtaugh's machine-managed troubleshoot.yaml. Two setup tools reach for it:
// setup.mcp-register (when it registers Murtaugh into a client that is itself a
// known provider, e.g. goose) and setup.agents (when it configures a claude_code
// agent). Keeping the read-modify-write in one place means both surfaces stay in
// lockstep instead of drifting into two copies.
package troubleshootcfg

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/miere/murtaugh/internal/tools/setup/internal/backup"
)

// RecordProvider adds provider to the providers list in the troubleshoot.yaml at
// path, creating the file if needed and preserving any other content. It returns
// whether the file changed (false when the provider was already listed).
// troubleshoot.yaml carries no user comments, so a plain map round-trip is safe.
func RecordProvider(path, provider string) (bool, error) {
	doc, err := readYAML(path)
	if err != nil {
		return false, err
	}
	ts, _ := doc["troubleshoot"].(map[string]any)
	if ts == nil {
		ts = map[string]any{}
		doc["troubleshoot"] = ts
	}
	providers := toStringSlice(ts["providers"])
	for _, p := range providers {
		if p == provider {
			return false, nil // already recorded; nothing to write
		}
	}
	providers = append(providers, provider)
	ts["providers"] = providers

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("ensure config dir: %w", err)
	}
	if _, err := backup.IfExists(path); err != nil {
		return false, err
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return false, fmt.Errorf("marshal troubleshoot.yaml: %w", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return false, fmt.Errorf("write %q: %w", path, err)
	}
	return true, nil
}

// readYAML returns the parsed object at path. A missing file yields an empty map
// so callers can populate it without branching.
func readYAML(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

// toStringSlice coerces a YAML-decoded value (which may be []any or []string)
// into []string, dropping non-string and empty entries.
func toStringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
