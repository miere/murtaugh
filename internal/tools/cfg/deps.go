// Package cfg implements the `murtaugh cfg …` admin surface: the supported way
// to configure Murtaugh now that the bulk of configuration lives in a database
// rather than hand-edited YAML. Every tool here is a registry Tool, so it is
// exposed identically over the CLI (`murtaugh cfg agent create …`) and MCP.
//
// The tools write typed config structs into the config.Store as JSON documents
// and, on every mutation, re-validate the whole assembled config — rejecting a
// change that would break it and rolling the store back. This is what lets the
// model configure Murtaugh safely instead of risking a malformed YAML edit.
package cfg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/miere/murtaugh/internal/config"
)

// Provider yields the open config store. It returns an error when the store is
// unavailable (e.g. a setup invocation that could not open the database), so a
// cfg tool fails cleanly rather than dereferencing nil.
type Provider func() (config.Store, error)

// NewProvider adapts a possibly-nil store into a Provider.
func NewProvider(s config.Store) Provider {
	return func() (config.Store, error) {
		if s == nil {
			return nil, errors.New("config store is unavailable")
		}
		return s, nil
	}
}

// validationBase is the placeholder credentials used when validating store
// content. Validate() requires the Slack tokens (a bootstrap-file concern, not
// a database one), so a cfg tool validating DB config supplies dummies to
// satisfy that check while every other rule runs for real.
var validationBase = config.Config{OAuth: config.OAuthConfig{AppToken: "x", BotToken: "x"}}

// validateStore loads and validates the whole config from the store, ignoring
// the bootstrap-only oauth requirement.
func validateStore(ctx context.Context, s config.Store) error {
	_, err := s.Load(ctx, validationBase)
	return err
}

// upsertItemValidated writes one collection entity and re-validates the whole
// config, rolling the store back to its prior state if the result would be
// invalid. This makes every mutation atomic with respect to validity.
func upsertItemValidated(ctx context.Context, s config.Store, section, name string, body any) error {
	prior, existed, err := s.GetItem(ctx, section, name)
	if err != nil {
		return err
	}
	if err := s.UpsertItem(ctx, section, name, body); err != nil {
		return err
	}
	if verr := validateStore(ctx, s); verr != nil {
		rollbackItem(ctx, s, section, name, prior, existed)
		return fmt.Errorf("change rejected — config would be invalid: %w", verr)
	}
	return nil
}

// putSingletonValidated is the singleton equivalent of upsertItemValidated.
func putSingletonValidated(ctx context.Context, s config.Store, key string, body any) error {
	prior, existed, err := s.GetSingleton(ctx, key)
	if err != nil {
		return err
	}
	if err := s.PutSingleton(ctx, key, body); err != nil {
		return err
	}
	if verr := validateStore(ctx, s); verr != nil {
		if existed {
			_ = s.PutSingleton(ctx, key, prior)
		}
		return fmt.Errorf("change rejected — config would be invalid: %w", verr)
	}
	return nil
}

func rollbackItem(ctx context.Context, s config.Store, section, name string, prior json.RawMessage, existed bool) {
	if existed {
		_ = s.UpsertItem(ctx, section, name, prior)
		return
	}
	_, _ = s.DeleteItem(ctx, section, name)
}

// --- argument accessors -----------------------------------------------------
//
// parseFlags only sets a key when its flag was passed, so "ok" distinguishes an
// explicitly-provided value from an omitted one — exactly what update semantics
// need (apply only what was given).

func stringArg(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

func intArg(args map[string]any, key string) (int, bool) {
	v, ok := args[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int64:
		return int(n), true
	case int:
		return n, true
	case float64:
		return int(n), true
	}
	return 0, false
}

func boolArg(args map[string]any, key string) (bool, bool) {
	v, ok := args[key]
	if !ok {
		return false, false
	}
	b, _ := v.(bool)
	return b, true
}

func arrayArg(args map[string]any, key string) ([]string, bool) {
	v, ok := args[key]
	if !ok {
		return nil, false
	}
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out, true
}

// envArg parses repeated --env KEY=VALUE flags into a map. A blank list clears
// the map; a malformed entry (no '=') is an error.
func envArg(args map[string]any, key string) (map[string]string, bool, error) {
	pairs, ok := arrayArg(args, key)
	if !ok {
		return nil, false, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, found := strings.Cut(p, "=")
		if !found || strings.TrimSpace(k) == "" {
			return nil, true, fmt.Errorf("--%s expects KEY=VALUE, got %q", strings.ReplaceAll(key, "_", "-"), p)
		}
		out[k] = v
	}
	return out, true, nil
}

func requireString(args map[string]any, key string) (string, error) {
	v, ok := stringArg(args, key)
	if !ok || strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("--%s is required", strings.ReplaceAll(key, "_", "-"))
	}
	return v, nil
}

// --- shared result renderers ------------------------------------------------

// okResult is a plain confirmation line for a mutation.
type okResult struct {
	Message string `json:"message"`
}

func (r okResult) String() string { return r.Message }

// listResult renders the names in a section (or the keys of a listing).
type listResult struct {
	Section string   `json:"section"`
	Names   []string `json:"names"`
}

func (r listResult) String() string {
	if len(r.Names) == 0 {
		return fmt.Sprintf("no %s configured", r.Section)
	}
	return fmt.Sprintf("%s (%d):\n  %s", r.Section, len(r.Names), strings.Join(r.Names, "\n  "))
}

// showResult renders a single entity's pretty-printed JSON body.
type showResult struct {
	Name string          `json:"name"`
	Body json.RawMessage `json:"body"`
}

func (r showResult) String() string {
	var buf strings.Builder
	if err := indentJSON(&buf, r.Body); err != nil {
		return string(r.Body)
	}
	return buf.String()
}

func indentJSON(b *strings.Builder, raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	enc := json.NewEncoder(b)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func sortedNames(m map[string]json.RawMessage) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
