package cli

import (
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func TestParseFlags_NilSchemaRejectsArgs(t *testing.T) {
	if _, err := parseFlags(nil, []string{"--foo", "bar"}); err == nil {
		t.Fatal("expected error when schema is nil but args are present")
	}
}

func TestParseFlags_KebabToSnake(t *testing.T) {
	schema := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"attachment_type": {Type: "string"},
		},
	}
	got, err := parseFlags(schema, []string{"--attachment-type", "markdown"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if got["attachment_type"] != "markdown" {
		t.Fatalf("got %v, want attachment_type=markdown", got)
	}
}

func TestParseFlags_CoercesIntegerAndBoolean(t *testing.T) {
	schema := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"count":   {Type: "integer"},
			"verbose": {Type: "boolean"},
		},
	}
	got, err := parseFlags(schema, []string{"--count", "5", "--verbose", "true"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if n, _ := got["count"].(int64); n != 5 {
		t.Fatalf("count = %v, want 5", got["count"])
	}
	if b, _ := got["verbose"].(bool); !b {
		t.Fatalf("verbose = %v, want true", got["verbose"])
	}
}

func TestParseFlags_UnknownFlagRejected(t *testing.T) {
	schema := &jsonschema.Schema{
		Type:       "object",
		Properties: map[string]*jsonschema.Schema{"name": {Type: "string"}},
	}
	if _, err := parseFlags(schema, []string{"--unknown", "value"}); err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

func TestParseFlags_MissingValue(t *testing.T) {
	schema := &jsonschema.Schema{
		Type:       "object",
		Properties: map[string]*jsonschema.Schema{"name": {Type: "string"}},
	}
	if _, err := parseFlags(schema, []string{"--name"}); err == nil {
		t.Fatal("expected error for flag without value")
	}
}

func TestParseFlags_RepeatedArrayFlagAccumulates(t *testing.T) {
	schema := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"args": {
				Type:  "array",
				Items: &jsonschema.Schema{Type: "string"},
			},
		},
	}
	got, err := parseFlags(schema, []string{"--args", "foo", "--args", "bar"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	xs, ok := got["args"].([]any)
	if !ok {
		t.Fatalf("args = %T, want []any", got["args"])
	}
	if len(xs) != 2 || xs[0] != "foo" || xs[1] != "bar" {
		t.Fatalf("args = %v, want [foo bar]", xs)
	}
}

func TestParseFlags_PositionalRejected(t *testing.T) {
	schema := &jsonschema.Schema{
		Type:       "object",
		Properties: map[string]*jsonschema.Schema{"name": {Type: "string"}},
	}
	if _, err := parseFlags(schema, []string{"bare-arg"}); err == nil {
		t.Fatal("expected error for positional argument")
	}
}
