package app

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRegistry_ContainsAllExpectedTools is the composition-root smoke test:
// it asserts that New wires up every tool the spec expects, with the right
// names and required-field schemas. Drift here means the binary ships with
// missing or mis-named tools.
func TestRegistry_ContainsAllExpectedTools(t *testing.T) {
	app := New(ModeCLI, nil, config.Config{}, nil, "/tmp/slack.yaml", "v0.0.0-test", discardLogger(), nil, Agents{})

	cases := []struct {
		name     string
		required []string
	}{
		{"ping", nil},
		{"jobs.define", []string{"name", "command"}},
		{"cfg.launchd", []string{}}, // every field defaults; the binary knows its own role
		{"cfg.migrate", nil},
		{"cfg.validate", nil},
		{"setup.update", []string{}},
		{"journal.query", []string{}},
		{"journal.stats", nil},
		{"journal.prune", nil},
	}

	for _, c := range cases {
		tool, ok := app.Registry().Get(c.name)
		if !ok {
			t.Errorf("registry missing %q", c.name)
			continue
		}
		schema := tool.InputSchema()
		if c.required == nil {
			if schema != nil {
				t.Errorf("%s: InputSchema = %+v, want nil", c.name, schema)
			}
			continue
		}
		if schema == nil || schema.Type != "object" {
			t.Errorf("%s: InputSchema type = %+v, want object", c.name, schema)
			continue
		}
		got := map[string]bool{}
		for _, r := range schema.Required {
			got[r] = true
		}
		for _, want := range c.required {
			if !got[want] {
				t.Errorf("%s: required missing %q (have %v)", c.name, want, schema.Required)
			}
		}
	}
}

// TestUsageLine_ListsFlatToolsNamespacesAndModes guards the bare-invocation
// help line. Regressions there mask missing tools or missing entry points.
func TestUsageLine_ListsFlatToolsNamespacesAndModes(t *testing.T) {
	line := New(ModeCLI, nil, config.Config{}, nil, "/tmp/slack.yaml", "v0.0.0-test", discardLogger(), nil, Agents{}).UsageLine()

	for _, want := range []string{
		"ping",
		"jobs <define>",
		"setup <update>",
		"slack <canvas|create_channel|fetch_msgs|fetch_reactions|send_msg|update_msg>",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("UsageLine missing %q in:\n%s", want, line)
		}
	}
	// Running the binary with no command starts the daemon, so `slack gateway`
	// is not a subcommand any more and must not be advertised as one.
	if strings.Contains(line, "gateway|") || strings.Contains(line, "|gateway") {
		t.Errorf("UsageLine still offers a `slack gateway` subcommand:\n%s", line)
	}
	if !strings.HasPrefix(line, "usage: murtaugh-gateway [command]; ") {
		t.Errorf("UsageLine prefix wrong: %q", line)
	}
}
