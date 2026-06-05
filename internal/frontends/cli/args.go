// Args parsing: turns --flag VALUE pairs into a map keyed by the property
// names declared in a tool's InputSchema. Boolean flags and positional
// arguments are intentionally unsupported — every Murtaugh tool flag carries
// a value. Validation against the schema (required fields, enums, types) is
// left to the tool or a future shared validator.
package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// parseFlags converts ["--kebab-case", "value", ...] pairs into
// map[string]any keyed by snake_case property names that the tool's schema
// declares. Schema property names are the source of truth. Array-typed
// properties accumulate across repeated --flag VALUE pairs
// (e.g. `--arg foo --arg bar` → []any{"foo", "bar"}).
func parseFlags(schema *jsonschema.Schema, args []string) (map[string]any, error) {
	if schema == nil {
		if len(args) > 0 {
			return nil, fmt.Errorf("unexpected arguments: %v", args)
		}
		return nil, nil
	}
	out := make(map[string]any, len(args)/2)
	for i := 0; i < len(args); i++ {
		raw := args[i]
		if !strings.HasPrefix(raw, "--") {
			return nil, fmt.Errorf("expected --flag, got %q", raw)
		}
		flag := strings.TrimPrefix(raw, "--")
		key := snakeFromKebab(flag)
		prop, ok := lookupProp(schema, key)
		if !ok {
			return nil, fmt.Errorf("unknown flag: --%s", flag)
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("flag --%s requires a value", flag)
		}
		val := args[i+1]
		i++
		v, err := coerce(val, prop)
		if err != nil {
			return nil, fmt.Errorf("--%s: %w", flag, err)
		}
		if propType(prop) == "array" {
			existing, _ := out[key].([]any)
			out[key] = append(existing, v)
			continue
		}
		out[key] = v
	}
	return out, nil
}

// snakeFromKebab maps --attachment-type → attachment_type, leaving names
// without dashes untouched.
func snakeFromKebab(s string) string { return strings.ReplaceAll(s, "-", "_") }

// lookupProp returns the schema for the given property name. Property names
// are matched case-sensitively against schema.Properties.
func lookupProp(schema *jsonschema.Schema, name string) (*jsonschema.Schema, bool) {
	if schema == nil || schema.Properties == nil {
		return nil, false
	}
	p, ok := schema.Properties[name]
	return p, ok
}

// propType returns the property's JSON Schema "type", consulting the legacy
// single-type field first and then the modern Types array.
func propType(prop *jsonschema.Schema) string {
	if prop == nil {
		return ""
	}
	if prop.Type != "" {
		return prop.Type
	}
	if len(prop.Types) > 0 {
		return prop.Types[0]
	}
	return ""
}

// coerce parses a raw CLI string into the Go type the property expects.
// Only the types Murtaugh tools actually use today are supported. Arrays of
// strings are emitted by repeating the flag (e.g. `--arg foo --arg bar`);
// each individual value is coerced via the items schema.
func coerce(raw string, prop *jsonschema.Schema) (any, error) {
	switch propType(prop) {
	case "", "string":
		return raw, nil
	case "integer":
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("expected integer, got %q", raw)
		}
		return n, nil
	case "number":
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("expected number, got %q", raw)
		}
		return f, nil
	case "boolean":
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("expected boolean, got %q", raw)
		}
		return b, nil
	case "array":
		if prop.Items != nil {
			return coerce(raw, prop.Items)
		}
		return raw, nil
	default:
		return raw, nil
	}
}
