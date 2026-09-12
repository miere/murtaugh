package nodeapp

import (
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/help"
)

// One reference covers both binaries, so the gateway's own docs test cannot see
// the node's tools at all. These are the same three guards applied to this half:
// help builds with no configuration, every tool has a section, and every flag a
// tool declares appears in it.

func TestHelpReferenceBuildsWithoutConfig(t *testing.T) {
	ref := HelpReference("test")
	if got := ref.Full(); len(got) == 0 {
		t.Fatal("HelpReference().Full() is empty")
	}
	if len(ref.Commands()) == 0 {
		t.Fatal("HelpReference() documents no commands")
	}
}

func TestEveryRegisteredToolIsDocumented(t *testing.T) {
	ref := HelpReference("test")
	for _, d := range nodeDocs(t) {
		section, ok := ref.Section(d.Name())
		if !ok {
			t.Errorf("no help section for registered tool %q", d.Name())
			continue
		}
		if !strings.HasPrefix(section, "## murtaugh ") {
			t.Errorf("%s: section does not start with a command header:\n%s", d.Name(), section)
		}
	}
}

func TestEveryFlagIsDocumented(t *testing.T) {
	ref := HelpReference("test")
	for _, d := range nodeDocs(t) {
		schema := d.InputSchema()
		if schema == nil {
			continue
		}
		section, ok := ref.Section(d.Name())
		if !ok {
			continue // reported by TestEveryRegisteredToolIsDocumented
		}
		for name := range schema.Properties {
			flag := "`--" + strings.ReplaceAll(name, "_", "-") + "`"
			if !strings.Contains(section, flag) {
				t.Errorf("%s: flag %s is not documented in its help section", d.Name(), flag)
			}
		}
	}
}

// A tool that enforces an argument in Invoke but omits it from Required tells
// every MCP client the call is valid without it, and the caller only finds out
// from a failed invocation.
func TestSchemaRequirednessIsDeclared(t *testing.T) {
	want := map[string][]string{
		"jobs.run":         {"name"},
		"cfg.mcp.set":      {"name"},
		"cfg.agent.create": {"name", "type"},
		"cfg.agent.update": {"name"},
	}
	byName := map[string]help.Doc{}
	for _, d := range nodeDocs(t) {
		byName[d.Name()] = d
	}
	for name, required := range want {
		d, ok := byName[name]
		if !ok {
			t.Errorf("tool %q is not registered", name)
			continue
		}
		got := map[string]bool{}
		for _, r := range d.InputSchema().Required {
			got[r] = true
		}
		for _, r := range required {
			if !got[r] {
				t.Errorf("%s: schema does not declare %q required, but Invoke enforces it", name, r)
			}
		}
	}
}

// The gateway must not carry the node admin's surface, or the split is only a
// convention.
func TestTheNodeCarriesNoGatewayAdminTool(t *testing.T) {
	for _, d := range nodeDocs(t) {
		switch d.Name() {
		case "cfg.access.set", "cfg.election.set", "cfg.node.split", "cfg.job.set":
			t.Errorf("the runtime binary registers %q, which belongs to the gateway admin", d.Name())
		}
		if strings.HasPrefix(d.Name(), "slack.") {
			t.Errorf("the runtime binary registers %q; only the gateway talks to Slack", d.Name())
		}
	}
}

// `--config` is a GLOBAL flag, stripped before any tool sees it, so a tool that
// declared one would document a flag that can never arrive.
func TestNoToolDeclaresAConfigFlag(t *testing.T) {
	for _, d := range nodeDocs(t) {
		schema := d.InputSchema()
		if schema == nil {
			continue
		}
		if _, ok := schema.Properties["config"]; ok {
			t.Errorf("%s declares a `config` argument; the global --config consumes it before the tool runs", d.Name())
		}
	}
}

func nodeDocs(t *testing.T) []help.Doc {
	t.Helper()
	docs := HelpDocs(Registry(config.Config{}, nil, "", "test"))
	if len(docs) == 0 {
		t.Fatal("registry is empty")
	}
	return docs
}
