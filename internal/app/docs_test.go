package app

import (
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/help"
)

// nonToolCommands are the `## murtaugh …` sections that no schema backs, and
// none can: the process modes dispatched in main before the registry is
// consulted, and `cfg`, which is an umbrella section introducing the whole
// cfg.* family rather than a command of its own.
var nonToolCommands = map[string]bool{
	"slack gateway": true,
	"mcp":           true,
	"help":          true,
	"cfg":           true,
}

// nodeOnlyCommands are documented commands the RUNTIME binary registers. One
// reference covers both binaries, so the gateway's registry cannot back them;
// internal/nodeapp/docs_test.go is what holds them to their schemas.
var nodeOnlyCommands = map[string]bool{
	"jobs run": true,
}

// TestHelpReferenceBuildsWithoutConfig is the load-bearing precondition for
// `murtaugh help` on an unconfigured machine: the reference builds the real
// registry from a zero config, so every tool must be constructible without a
// store, a journal, a Slack token or an interaction broker.
func TestHelpReferenceBuildsWithoutConfig(t *testing.T) {
	ref := HelpReference("test")
	if got := ref.Full(); len(got) == 0 {
		t.Fatal("HelpReference().Full() is empty")
	}
	if len(ref.Commands()) == 0 {
		t.Fatal("HelpReference() documents no commands")
	}
}

// TestEveryRegisteredToolIsDocumented replaces the hand-maintained command
// list this test file used to carry. That list was itself the drift it was
// meant to prevent — it never grew entries for ask, present_plan, restart or
// slack.canvas. Walking the registry cannot fall behind the registry.
func TestEveryRegisteredToolIsDocumented(t *testing.T) {
	ref := HelpReference("test")
	for _, d := range helpDocs(t) {
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

// TestEveryFlagIsDocumented is the guard that makes adding a parameter
// self-documenting: every property a tool declares must appear as a flag in
// its rendered section, whether the section is generated or spliced into
// hand-written prose.
func TestEveryFlagIsDocumented(t *testing.T) {
	ref := HelpReference("test")
	for _, d := range helpDocs(t) {
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

// TestNoProseSectionOutlivesItsTool catches the other direction of drift: a
// `## murtaugh …` block in cli-help.md for a command that no longer exists.
// The generated half cannot produce one of these, so only a stale hand-written
// section can.
func TestNoProseSectionOutlivesItsTool(t *testing.T) {
	for _, cmd := range HelpReference("test").Orphans() {
		if !nonToolCommands[cmd] && !nodeOnlyCommands[cmd] {
			t.Errorf("cli-help.md documents %q, but no tool is registered under that name", cmd)
		}
	}
}

// TestSchemaRequirednessIsDeclared is a spot-check on the fix that made the
// cfg family declare its required flags. A tool that enforces an argument in
// Invoke but omits it from Required tells every MCP client the call is valid
// without it, and the caller only finds out from a failed invocation.
func TestSchemaRequirednessIsDeclared(t *testing.T) {
	want := map[string][]string{
		"cfg.job.set":    {"name"},
		"cfg.import":     {"file"},
		"cfg.db.migrate": {"to"},
	}
	byName := map[string]help.Doc{}
	for _, d := range helpDocs(t) {
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

// `--config` is a GLOBAL flag, stripped from the whole command line before any
// tool sees it, so a tool that declared one would document a flag that can
// never arrive — and passing it would silently retarget the whole invocation.
// This is why `cfg node split` names its destination `--dest`.
func TestNoToolDeclaresAConfigFlag(t *testing.T) {
	for _, d := range helpDocs(t) {
		schema := d.InputSchema()
		if schema == nil {
			continue
		}
		if _, ok := schema.Properties["config"]; ok {
			t.Errorf("%s declares a `config` argument; the global --config consumes it before the tool runs", d.Name())
		}
	}
}

// helpDocs returns the documented tool set, failing the test rather than
// panicking if the registry cannot be built.
func helpDocs(t *testing.T) []help.Doc {
	t.Helper()
	docs := HelpDocs(docsRegistry("test"))
	if len(docs) == 0 {
		t.Fatal("registry is empty")
	}
	return docs
}
