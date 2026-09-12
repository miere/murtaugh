package help

import (
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// fakeDoc is a stand-in tool. The reference only ever reads these three
// methods, so tests can document a command without building a real tool.
type fakeDoc struct {
	name   string
	desc   string
	schema *jsonschema.Schema
}

func (f fakeDoc) Name() string                    { return f.name }
func (f fakeDoc) Description() string             { return f.desc }
func (f fakeDoc) InputSchema() *jsonschema.Schema { return f.schema }
func doc(name string, s *jsonschema.Schema) fakeDoc {
	return fakeDoc{name: name, desc: "Does a thing.", schema: s}
}

func TestFullNonEmpty(t *testing.T) {
	full := New(nil).Full()
	if !strings.Contains(full, "# murtaugh — command-line reference") {
		t.Fatalf("Full() missing document title; got %d bytes", len(full))
	}
}

func TestSectionLookup(t *testing.T) {
	ref := New([]Doc{doc("present_plan", nil)})
	cases := []struct {
		name string
		want string // header line the section must start with
	}{
		{"ping", "## murtaugh ping"},
		{"jobs run", "## murtaugh jobs run"},
		{"jobs.run", "## murtaugh jobs run"}, // dotted registry form
		{"jobs_run", "## murtaugh jobs run"}, // MCP published form
		{"slack send_msg", "## murtaugh slack send_msg"},
		{"slack.send_msg", "## murtaugh slack send_msg"},
		{"  Jobs   Define ", "## murtaugh jobs define"}, // whitespace/case tolerant
		{"setup update", "## murtaugh setup update"},
		{"present_plan", "## murtaugh present_plan"}, // generated, no prose section
	}
	for _, tc := range cases {
		got, ok := ref.Section(tc.name)
		if !ok {
			t.Errorf("Section(%q) not found", tc.name)
			continue
		}
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("Section(%q) = %q…, want prefix %q", tc.name, firstLine(got), tc.want)
		}
	}
}

// TestSectionDoesNotBleed ensures a section stops at the next command header.
func TestSectionDoesNotBleed(t *testing.T) {
	got, ok := New(nil).Section("ping")
	if !ok {
		t.Fatal("Section(ping) not found")
	}
	if strings.Contains(got, "## murtaugh jobs run") {
		t.Errorf("Section(ping) bled into the next command section:\n%s", got)
	}
}

func TestSectionMissing(t *testing.T) {
	ref := New(nil)
	if _, ok := ref.Section("does-not-exist"); ok {
		t.Error("Section(does-not-exist) reported found")
	}
	if _, ok := ref.Section(""); ok {
		t.Error("Section(empty) reported found")
	}
}

func TestRender(t *testing.T) {
	ref := New(nil)
	if ref.Render(nil) != ref.Full() {
		t.Error("Render(nil) should return the full document")
	}
	if got := ref.Render([]string{"slack", "send-msg"}); !strings.HasPrefix(got, "## murtaugh slack send_msg") {
		t.Errorf("Render([slack send_msg]) = %q…", firstLine(got))
	}
	if got := ref.Render([]string{"nope"}); !strings.Contains(got, "No help section") {
		t.Error("Render of an unknown command should fall back with a notice")
	}
}

// TestSpliceReplacesHandwrittenTable is the whole point of the package: the
// prose survives, but the flag table is the tool's schema and not whatever a
// human last typed into cli-help.md.
func TestSpliceReplacesHandwrittenTable(t *testing.T) {
	d := doc("jobs.run", &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name":      {Type: "string", Description: "Job key."},
			"brand_new": {Type: "string", Description: "A flag the prose has never heard of."},
		},
		Required: []string{"name"},
	})
	got, ok := New([]Doc{d}).Section("jobs run")
	if !ok {
		t.Fatal("Section(jobs run) not found")
	}
	if !strings.Contains(got, "`--brand-new`") {
		t.Errorf("generated table missing the new flag:\n%s", got)
	}
	// Hand-written prose either side of the table must be untouched.
	if !strings.Contains(got, "Default timeout is **10 minutes**") {
		t.Errorf("splice dropped the hand-written notes:\n%s", got)
	}
	if !strings.Contains(got, "murtaugh jobs run --name nightly-backup") {
		t.Errorf("splice dropped the hand-written example:\n%s", got)
	}
	if strings.Count(got, "| Flag") != 1 {
		t.Errorf("expected exactly one flag table, got %d:\n%s", strings.Count(got, "| Flag"), got)
	}
}

// TestSpliceKeepsBlankLineAfterTable guards the formatting bug where the
// replaced table fused onto the paragraph below it.
func TestSpliceKeepsBlankLineAfterTable(t *testing.T) {
	d := doc("jobs.run", &jsonschema.Schema{
		Type:       "object",
		Properties: map[string]*jsonschema.Schema{"name": {Type: "string", Description: "Job key."}},
		Required:   []string{"name"},
	})
	got, _ := New([]Doc{d}).Section("jobs run")
	lines := strings.Split(got, "\n")
	for i, ln := range lines {
		if strings.HasPrefix(ln, "|") && i+1 < len(lines) {
			next := lines[i+1]
			if !strings.HasPrefix(next, "|") && strings.TrimSpace(next) != "" {
				t.Fatalf("table row is not followed by a blank line:\n%q\n%q", ln, next)
			}
		}
	}
}

// TestSpliceDropsStaleTableForFlaglessTool covers a tool that lost its last
// parameter: the hand-written table must go, not linger as a lie.
func TestSpliceDropsStaleTableForFlaglessTool(t *testing.T) {
	got, ok := New([]Doc{doc("jobs.run", nil)}).Section("jobs run")
	if !ok {
		t.Fatal("Section(jobs run) not found")
	}
	if strings.Contains(got, "| Flag") {
		t.Errorf("a tool with no schema should have no flag table:\n%s", got)
	}
	if !strings.Contains(got, "Default timeout is **10 minutes**") {
		t.Errorf("dropping the table should not take the prose with it:\n%s", got)
	}
}

// TestFullAppendsUndocumentedTools is the guard that replaces the old
// hand-maintained command list: a tool with no prose section still appears.
func TestFullAppendsUndocumentedTools(t *testing.T) {
	full := New([]Doc{doc("brand.new-tool", nil)}).Full()
	if !strings.Contains(full, "## murtaugh brand new-tool") {
		t.Error("Full() omitted a tool with no hand-written section")
	}
}

func TestCommandsCoversProseAndTools(t *testing.T) {
	cmds := New([]Doc{doc("brand.new-tool", nil)}).Commands()
	index := map[string]bool{}
	for _, c := range cmds {
		index[c] = true
	}
	for _, want := range []string{"ping", "slack send_msg", "brand new-tool"} {
		if !index[want] {
			t.Errorf("Commands() missing %q", want)
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func TestSectionMatchesAnySpelling(t *testing.T) {
	ref := New([]Doc{doc("slack.send_msg", nil)})
	for _, name := range []string{"slack send_msg", "slack.send_msg", "slack_send_msg", "slack send_msg"} {
		got, ok := ref.Section(name)
		if !ok || !strings.HasPrefix(got, "## murtaugh slack send_msg") {
			t.Errorf("Section(%q) = %q, %v; want the hand-written send-msg section", name, firstLine(got), ok)
		}
	}
}

func TestOrphansListsProseWithNoTool(t *testing.T) {
	orphans := New([]Doc{doc("ping", nil)}).Orphans()
	index := map[string]bool{}
	for _, o := range orphans {
		index[o] = true
	}
	if index["ping"] {
		t.Error("ping has a tool but was reported as an orphan")
	}
	if !index["slack send_msg"] {
		t.Errorf("slack send_msg has no tool here but was not reported: %v", orphans)
	}
}
