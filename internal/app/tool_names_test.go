package app

import (
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/miere/murtaugh/internal/agent/claudecode"
	"github.com/miere/murtaugh/internal/tools"
	"github.com/miere/murtaugh/internal/tools/files"
	"github.com/miere/murtaugh/internal/toolset"
)

// A model guesses the names it hasn't seen from the ones it has, so every tool we
// publish follows one pattern. Proxied MCP servers are someone else's and exempt.
func TestToolNamesAreSnakeCase(t *testing.T) {
	re := regexp.MustCompile(`^[a-z]+(_[a-z]+)*$`)
	for _, tl := range ourTools(t) {
		if published := strings.ReplaceAll(tl.Name(), ".", "_"); !re.MatchString(published) {
			t.Errorf("tool %q publishes as %q, which does not match %s", tl.Name(), published, re)
		}
	}
}

func ourTools(t *testing.T) []tools.Tool {
	t.Helper()
	root, err := files.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	groups := make([]string, 0, len(toolset.NativeGroups))
	for _, g := range toolset.NativeGroups {
		groups = append(groups, g.Name)
	}
	native, _, err := toolset.Resolve(groups, nil, toolset.Deps{Root: root, ManagedSkillsFS: fstest.MapFS{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(native) < len(groups) {
		t.Fatalf("resolved %d native tools for %d groups; some group was dropped", len(native), len(groups))
	}
	return append(docsRegistry("test").All(), native...)
}

// An alias keyed by a name we no longer register would silently stop applying,
// and the model would lose the tool it reaches for by reflex.
func TestBackendAliasesNameRegisteredTools(t *testing.T) {
	for name, alias := range claudecode.ToolAliases {
		if _, ok := docsRegistry("test").Get(name); !ok {
			t.Errorf("claude_code publishes %q as %q, but no tool is registered as %q", name, alias, name)
		}
	}
}
