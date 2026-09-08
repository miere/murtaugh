package agentbuild

import (
	"context"
	"slices"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/mcpbridge"
	"github.com/miere/murtaugh/internal/tools"
	"github.com/miere/murtaugh/internal/tools/files"
)

// resolvedFor builds a ResolvedAgent for aggregator tests: a workspace root from
// dir (nil when dir is empty) and the given effective allowlist. In-package, so
// it sets the unexported fields directly.
func resolvedFor(t *testing.T, dir string, toolList ...string) ResolvedAgent {
	t.Helper()
	var root *files.Root
	if dir != "" {
		r, err := files.NewRoot(dir)
		if err != nil {
			t.Fatalf("files.NewRoot(%q): %v", dir, err)
		}
		root = r
	}
	return ResolvedAgent{name: "test", root: root, tools: toolList}
}

type fakeTool struct{ name string }

func (f fakeTool) Name() string                                        { return f.name }
func (f fakeTool) Description() string                                 { return "fake" }
func (f fakeTool) InputSchema() *jsonschema.Schema                     { return nil }
func (f fakeTool) Invoke(context.Context, map[string]any) (any, error) { return "ok", nil }

func registryWith(names ...string) *tools.Registry {
	reg := tools.NewRegistry()
	for _, n := range names {
		reg.Register(fakeTool{name: n})
	}
	return reg
}

func toolNames(ts []tools.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

func TestResolveBuiltinsCuratesAndStripsGroups(t *testing.T) {
	reg := registryWith("ping", "ask", "slack.send_msg", "slack.fetch_msgs", "setup.env", "restart")
	// Allow a namespace (slack), an exact tool (ask), a curated-out namespace
	// (setup), and a native-only group (files) that must not synthesize anything.
	// A real workdir is supplied so files survives the seam's prune and we are
	// genuinely exercising the ACP strip (not the prune).
	got, err := resolveBuiltins(reg, resolvedFor(t, t.TempDir(), "ask", "slack", "setup", "files"))
	if err != nil {
		t.Fatalf("resolveBuiltins: %v", err)
	}
	names := toolNames(got)
	for _, want := range []string{"ask", "slack.send_msg", "slack.fetch_msgs"} {
		if !slices.Contains(names, want) {
			t.Fatalf("expected %q in resolved set, got %v", want, names)
		}
	}
	if slices.Contains(names, "setup.env") {
		t.Fatalf("setup.* must be curated out of the bridge surface, got %v", names)
	}
	if slices.Contains(names, "ping") {
		t.Fatalf("ping was not allowed, should be absent, got %v", names)
	}
	// "files" is a native-only synthesized group; nothing should appear for it.
	for _, n := range names {
		if n == "files" {
			t.Fatalf("the files group must not be served to an ACP agent, got %v", names)
		}
	}
}

// TestResolveBuiltinsKeepsAttach is the unit-level guard for the bug: `attach` is
// workdir-rooted but NOT native-only, so an ACP agent with a workspace must be
// served the attach tool — while files/terminal/skills stay stripped.
func TestResolveBuiltinsKeepsAttach(t *testing.T) {
	reg := registryWith("ask")
	got, err := resolveBuiltins(reg, resolvedFor(t, t.TempDir(), "ask", "attach", "files", "terminal", "skills"))
	if err != nil {
		t.Fatalf("resolveBuiltins: %v", err)
	}
	names := toolNames(got)
	if !slices.Contains(names, "attach") {
		t.Fatalf("attach must be served to ACP agents, got %v", names)
	}
	for _, banned := range []string{"files", "terminal", "skills"} {
		if slices.Contains(names, banned) {
			t.Fatalf("native-only group %q must be stripped, got %v", banned, names)
		}
	}
}

func TestBridgeUnsafe(t *testing.T) {
	for _, n := range []string{"setup", "setup.env", "setup.slack"} {
		if !bridgeUnsafe(n) {
			t.Fatalf("%q should be bridge-unsafe", n)
		}
	}
	for _, n := range []string{"ask", "slack.send_msg", "setupx", "restart"} {
		if bridgeUnsafe(n) {
			t.Fatalf("%q should be bridge-safe", n)
		}
	}
}

// The aggregator must read the registry when a SESSION is registered, not when
// it is built. This is the acp/claude_code half of #194 and the one a rig that
// only models `native` cannot see.
//
// On a runtime node the registry handed to the agent builder is
// nodeserve.ToolProxy's, and it is empty at that moment: the node builds its
// agent before it has a gateway connection, and the proxy is filled at the
// handshake. An aggregator that snapshotted its built-ins at construction served
// zero tools forever — the exact regression this item exists to end, in the two
// backends it was written for, while `native` looked fine because it resolves
// inside its own Initialize, after the handshake.
//
// The registry below is therefore filled AFTER the aggregator is built, in that
// order deliberately.
func TestTheAggregatorReadsTheRegistryWhenASessionStartsNotWhenItIsBuilt(t *testing.T) {
	reg := tools.NewRegistry() // a node's proxy registry: empty until the handshake
	srv := mcpbridge.NewServer("/tmp/murtaugh-test-agg3.sock", nil)
	aggr, err := newACPAggregator(srv, reg, resolvedFor(t, "", "ask", "ping"), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("newACPAggregator: %v", err)
	}

	// What nodeserve.ToolProxy.refresh does at the handshake, one hop before the
	// agent's first session.
	reg.Register(fakeTool{name: "ask"})
	reg.Register(fakeTool{name: "ping"})

	if _, _, err := aggr.RegisterSession(agent.SessionMetadata{ChannelID: "C1", ThreadTS: "1.2"}, nil); err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	served, err := aggr.resolvedToolset()
	if err != nil {
		t.Fatalf("resolvedToolset: %v", err)
	}
	got := toolNames(served)
	for _, want := range []string{"ask", "ping"} {
		if !slices.Contains(got, want) {
			t.Fatalf("an acp/claude_code agent was served %v, so it reaches no Murtaugh tools at all on a node", got)
		}
	}
}

func TestACPAggregatorRegisterSession(t *testing.T) {
	reg := registryWith("ask")
	srv := mcpbridge.NewServer("/tmp/murtaugh-test-agg.sock", nil)
	aggr, err := newACPAggregator(srv, reg, resolvedFor(t, "", "ask"), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("newACPAggregator: %v", err)
	}

	spec, release, err := aggr.RegisterSession(agent.SessionMetadata{ChannelID: "C1", ThreadTS: "1.2"}, nil)
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	if spec.Name != "murtaugh" || len(spec.Args) != 1 || spec.Args[0] != mcpbridge.Subcommand {
		t.Fatalf("unexpected spec command shape: %+v", spec)
	}
	if spec.Env[mcpbridge.EnvSocket] != srv.SocketPath() {
		t.Fatalf("spec env socket = %q, want %q", spec.Env[mcpbridge.EnvSocket], srv.SocketPath())
	}
	if spec.Env[mcpbridge.EnvToken] == "" {
		t.Fatal("spec env is missing the session token")
	}
	if release == nil {
		t.Fatal("expected a non-nil release")
	}
	release() // must not panic; drops the token
}

func TestACPAggregatorToolsetAndClose(t *testing.T) {
	reg := registryWith("ask", "slack.send_msg")
	srv := mcpbridge.NewServer("/tmp/murtaugh-test-agg2.sock", nil)
	// No external MCP servers configured: the toolset is just the built-ins.
	aggr, err := newACPAggregator(srv, reg, resolvedFor(t, "", "ask", "slack"), nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("newACPAggregator: %v", err)
	}
	// Close before any session opened the manager must be a safe no-op.
	if err := aggr.Close(); err != nil {
		t.Fatalf("Close before use: %v", err)
	}
	served, err := aggr.resolvedToolset()
	if err != nil {
		t.Fatalf("resolvedToolset: %v", err)
	}
	got := toolNames(served)
	if len(got) != 2 || !slices.Contains(got, "ask") || !slices.Contains(got, "slack.send_msg") {
		t.Fatalf("resolved toolset = %v, want the two built-ins", got)
	}
	// Close after the (empty) manager opened must also succeed.
	if err := aggr.Close(); err != nil {
		t.Fatalf("Close after use: %v", err)
	}
}
