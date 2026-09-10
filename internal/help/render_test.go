package help

import (
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

func TestFlagTableIsEmptyForNoParameters(t *testing.T) {
	if got := FlagTable(nil); got != "" {
		t.Errorf("FlagTable(nil) = %q, want empty", got)
	}
	if got := FlagTable(&jsonschema.Schema{Type: "object"}); got != "" {
		t.Errorf("FlagTable(no properties) = %q, want empty", got)
	}
}

// TestFlagTableOrderIsStable pins the documentation order — required flags in
// declaration order, then the rest alphabetically. Without it, Go's random map
// iteration would reshuffle every table on every run.
func TestFlagTableOrderIsStable(t *testing.T) {
	schema := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"zulu":  {Type: "string"},
			"alpha": {Type: "string"},
			"to":    {Type: "string"},
			"body":  {Type: "string"},
		},
		Required: []string{"body", "to"},
	}
	want := []string{"--body", "--to", "--alpha", "--zulu"}
	first := FlagTable(schema)
	for i := 0; i < 20; i++ {
		if FlagTable(schema) != first {
			t.Fatal("FlagTable is not deterministic across renders")
		}
	}
	pos := -1
	for _, flag := range want {
		at := strings.Index(first, "`"+flag+"`")
		if at < 0 {
			t.Fatalf("flag %s missing from table:\n%s", flag, first)
		}
		if at < pos {
			t.Fatalf("flag %s out of order:\n%s", flag, first)
		}
		pos = at
	}
}

func TestFlagTableRendersSchemaFacts(t *testing.T) {
	schema := &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name":  {Type: "string", Description: "The name"},
			"arg":   {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "An argument"},
			"as":    {Type: "string", Enum: []any{"bot", "admin"}, Description: "Sender identity"},
			"force": {Type: "boolean", Description: "Overwrite"},
			"count": {Type: "integer", Description: "How many"},
		},
		Required: []string{"name"},
	}
	got := FlagTable(schema)
	for _, want := range []string{
		"| `--name`", "| yes ", "The name.", // description gets a full stop
		"string[]", "Repeatable — pass the flag once per value.",
		"enum", "One of: `bot`, `admin`.",
		"boolean", "Needs an explicit value (`true`/`false`).",
		"integer",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("FlagTable missing %q:\n%s", want, got)
		}
	}
}

// TestFlagTableDoesNotRepeatRepeatable covers the case where the tool author
// already wrote "(repeatable)" into the description by hand.
func TestFlagTableDoesNotRepeatRepeatable(t *testing.T) {
	got := FlagTable(&jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"arg": {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "command argument (repeatable)"},
		},
	})
	if strings.Contains(got, "Repeatable — pass") {
		t.Errorf("added a repetition note the description already carried:\n%s", got)
	}
}

func TestUsageLine(t *testing.T) {
	cases := []struct {
		name string
		doc  Doc
		want string
	}{
		{
			name: "no parameters",
			doc:  doc("ping", nil),
			want: "murtaugh ping",
		},
		{
			name: "required flags are spelled out",
			doc: doc("slack.send-msg", &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"body":   {Type: "string"},
					"to":     {Type: "string"},
					"thread": {Type: "string"},
				},
				Required: []string{"body", "to"},
			}),
			want: "murtaugh slack send-msg --body <string> --to <string> [flags]",
		},
		{
			name: "optional only",
			doc: doc("cfg.export", &jsonschema.Schema{
				Type:       "object",
				Properties: map[string]*jsonschema.Schema{"file": {Type: "string"}},
			}),
			want: "murtaugh cfg export [flags]",
		},
		{
			name: "required enum shows its values",
			doc: doc("cfg.db.migrate", &jsonschema.Schema{
				Type:       "object",
				Properties: map[string]*jsonschema.Schema{"to": {Type: "string", Enum: []any{"postgres", "sqlite"}}},
				Required:   []string{"to"},
			}),
			want: "murtaugh cfg db migrate --to <postgres|sqlite>",
		},
	}
	for _, tc := range cases {
		if got := UsageLine(tc.doc); got != tc.want {
			t.Errorf("%s: UsageLine() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestRenderSectionNamesTheMCPForm matters because the three spellings of a
// tool are the thing a model gets wrong: it sees `slack_send-msg` in its tool
// list and types that at a shell.
func TestRenderSectionNamesTheMCPForm(t *testing.T) {
	got := RenderSection(doc("slack.send-msg", nil))
	if !strings.Contains(got, "`slack_send-msg`") {
		t.Errorf("section does not name the MCP form:\n%s", got)
	}
	if plain := RenderSection(doc("ping", nil)); strings.Contains(plain, "Over MCP") {
		t.Errorf("a tool whose name needs no normalisation should not mention MCP:\n%s", plain)
	}
}

func TestRenderSectionIncludesHeaderDescriptionAndUsage(t *testing.T) {
	got := RenderSection(doc("jobs.run", &jsonschema.Schema{
		Type:       "object",
		Properties: map[string]*jsonschema.Schema{"name": {Type: "string", Description: "Job key"}},
		Required:   []string{"name"},
	}))
	for _, want := range []string{"## murtaugh jobs run", "Does a thing.", "| `--name`", "murtaugh jobs run --name <string>"} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderSection missing %q:\n%s", want, got)
		}
	}
}

func TestWrapKeepsWordsIntact(t *testing.T) {
	got := wrap(strings.Repeat("word ", 40), 20)
	for _, line := range strings.Split(got, "\n") {
		if len(line) > 20 {
			t.Errorf("line exceeds width: %q", line)
		}
	}
	if strings.ReplaceAll(got, "\n", " ") != strings.TrimSpace(strings.Repeat("word ", 40)) {
		t.Error("wrap altered the text")
	}
}
