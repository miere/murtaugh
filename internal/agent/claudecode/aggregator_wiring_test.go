package claudecode

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
)

// fakeAggregator records RegisterSession / release calls so we can assert the
// claude_code backend wires Murtaugh's tool bridge per session.
type fakeAggregator struct {
	registered int
	released   int
	meta       agent.SessionMetadata
}

func (f *fakeAggregator) RegisterSession(meta agent.SessionMetadata) (agent.MCPServerSpec, func(), error) {
	f.registered++
	f.meta = meta
	spec := agent.MCPServerSpec{
		Name:    "murtaugh",
		Command: "/bin/murtaugh",
		Args:    []string{"mcp-bridge"},
		Env:     map[string]string{"MURTAUGH_BRIDGE_SOCKET": "/tmp/s.sock", "MURTAUGH_BRIDGE_TOKEN": "tok"},
	}
	return spec, func() { f.released++ }, nil
}

func TestMcpConfigArg_ShapesClaudeMcpConfig(t *testing.T) {
	spec := agent.MCPServerSpec{
		Name:    "murtaugh",
		Command: "/bin/m",
		Args:    []string{"mcp-bridge"},
		Env:     map[string]string{"MURTAUGH_BRIDGE_TOKEN": "tok"},
	}
	got, err := mcpConfigArg(spec)
	if err != nil {
		t.Fatalf("mcpConfigArg: %v", err)
	}
	var parsed struct {
		McpServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, got)
	}
	m, ok := parsed.McpServers["murtaugh"]
	if !ok {
		t.Fatalf("expected a \"murtaugh\" server entry, got: %s", got)
	}
	if m.Command != "/bin/m" || len(m.Args) != 1 || m.Args[0] != "mcp-bridge" {
		t.Fatalf("wrong command/args: %+v", m)
	}
	if m.Env["MURTAUGH_BRIDGE_TOKEN"] != "tok" {
		t.Fatalf("env not carried: %+v", m.Env)
	}
}

// TestNewSession_RegistersAggregatorAndReleasesOnClose proves the claude_code
// backend registers each session with the aggregator (so the bridge is advertised
// to the process) and releases the token when the session closes.
func TestNewSession_RegistersAggregatorAndReleasesOnClose(t *testing.T) {
	agg := &fakeAggregator{}
	c := newHelperClient(t, "basic", Options{Aggregator: agg})
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	sess, err := c.NewSession(ctx, agent.SessionMetadata{ChannelID: "C1", ThreadTS: "1.1"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if agg.registered != 1 {
		t.Fatalf("RegisterSession called %d times, want 1", agg.registered)
	}
	if agg.meta.ChannelID != "C1" {
		t.Fatalf("session metadata not forwarded to aggregator: %+v", agg.meta)
	}
	c.CloseSession(sess.ID)
	if agg.released != 1 {
		t.Fatalf("aggregator release called %d times, want 1", agg.released)
	}
}

// TestNewSession_NoAggregatorIsFine confirms the backend still works with no
// aggregator wired (the pre-fix behaviour) — no registration, no panic.
func TestNewSession_NoAggregatorIsFine(t *testing.T) {
	c := newHelperClient(t, "basic", Options{})
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := c.NewSession(ctx, agent.SessionMetadata{ChannelID: "C1"}); err != nil {
		t.Fatalf("NewSession without aggregator: %v", err)
	}
}
