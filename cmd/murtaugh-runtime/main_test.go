package main

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/mcpbridge"
	"github.com/miere/murtaugh/internal/nodeserve"
)

// The kind is defaulted rather than stored, so printing the raw field would show
// a blank for every agent that never set one.
func TestReportNamesEachAgentAndItsResolvedKind(t *testing.T) {
	cfg := config.Config{
		BaseDir: "/tmp/murtaugh-runtime-test",
		Agents: map[string]config.AgentProfile{
			"zeta":    {ClaudeCode: &config.ClaudeCodeProfile{}},
			"default": {},
		},
	}

	var out strings.Builder
	reportAgents(&out, cfg, []string{"wss://gateway.example:8443"})
	got := out.String()

	for _, want := range []string{
		"agents: 2",
		"default (native)",
		"zeta (claude_code)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report does not contain %q; got:\n%s", want, got)
		}
	}
	if strings.Index(got, "default (") > strings.Index(got, "zeta (") {
		t.Errorf("agents are not listed in name order; got:\n%s", got)
	}
}

// An empty list would read like a successful enrolment.
func TestReportSaysSoWhenNoAgentIsConfigured(t *testing.T) {
	var out strings.Builder
	reportAgents(&out, config.Config{BaseDir: "/tmp/murtaugh-runtime-test"}, nil)

	if got := out.String(); !strings.Contains(got, "agents: none configured") {
		t.Errorf("report does not say the machine has no agents; got:\n%s", got)
	}
}

// A missing hook fails silently, and a hook wired to the wrong sink still passes a
// nil check, so this compares identities.
func TestANodeContributesBothOfItsHooks(t *testing.T) {
	cfg := config.Config{Agents: map[string]config.AgentProfile{
		"default": {},
		"zeta":    {ClaudeCode: &config.ClaudeCodeProfile{}},
	}}
	gate := nodeserve.NewToolGate(nil)
	background := nodeserve.NewBackgroundSink(nil)

	hooks := nodeHooks(cfg, gate, background)

	if !hooks.Chat {
		t.Error("a node built no chat surface, so nothing would ever prompt the agent it serves")
	}
	for name := range cfg.Agents {
		if hooks.Approvers[name] != agentruntime.Approver(gate) {
			t.Errorf("agent %q was built without the node's approval gate; its tool calls would run unprompted", name)
		}
	}
	if hooks.BackgroundEvents == nil {
		t.Fatal("a node built no background sink; a claude_code stretch that goes quiet would produce no notice")
	}
	if got, want := funcPointer(hooks.BackgroundEvents), funcPointer(background.Handle); got != want {
		t.Fatal("the background hook is not the connection-bound sink, so its events cannot reach the link")
	}
}

func funcPointer(fn any) uintptr { return reflect.ValueOf(fn).Pointer() }

// A supervisor must see a non-zero exit, not a clean start that serves nothing.
// It drives run() because the sentinel is worthless if a later edit stops returning it.
func TestRunRefusesToStartWithNoGatewayToDial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	err := run([]string{"--config", path})

	if !errors.Is(err, errNoGateway) {
		t.Fatalf("run() = %v, want errNoGateway — the binary must not exit 0 having served nothing", err)
	}
	if !strings.Contains(err.Error(), "a node dials in") {
		t.Errorf("run() = %q, want it to say which end dials", err)
	}
}

// On a node os.Executable() is murtaugh-runtime, so without this branch every
// acp/claude_code agent silently loses its Murtaugh tools.
func TestTheBridgeSubcommandIsDispatchedOnANode(t *testing.T) {
	t.Setenv(mcpbridge.EnvSocket, "")
	t.Setenv(mcpbridge.EnvToken, "")

	err := run([]string{mcpbridge.Subcommand})

	if err == nil {
		t.Fatal("mcp-bridge with no socket in the environment did not fail")
	}
	if strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("murtaugh-runtime still rejects `mcp-bridge` as a stray argument, so an acp agent on this node spawns a bridge that dies: %v", err)
	}
	if !strings.Contains(err.Error(), mcpbridge.EnvSocket) {
		t.Fatalf("mcp-bridge failed for some reason other than its missing environment: %v", err)
	}
}

func TestANodeServesItsOwnTools(t *testing.T) {
	registry := nodeTools(nodeserve.NewSignIns(nil))
	var names []string
	for _, tool := range registry.All() {
		names = append(names, tool.Name())
	}
	sort.Strings(names)
	want := []string{"ask", "auth.request", "help", "ping", "present_plan", "version"}
	if !slices.Equal(names, want) {
		t.Fatalf("the node registers %v, want %v", names, want)
	}

	help, _ := registry.Get("help")
	out, err := help.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("help: %v", err)
	}
	for _, name := range want {
		if !strings.Contains(out.(string), strings.ReplaceAll(name, ".", " ")) {
			t.Errorf("help does not list %q:\n%s", name, out)
		}
	}
}

func TestANodesSignInWithNoConversationGoesToItsOwner(t *testing.T) {
	registry := nodeTools(nodeserve.NewSignIns(nil))
	tool, ok := registry.Get("auth.request")
	if !ok {
		t.Fatal("the node registers no auth.request")
	}
	_, err := tool.Invoke(context.Background(), map[string]any{"tool": "vendor-mcp", "profile": "custom", "command": "true"})
	if err == nil || strings.Contains(err.Error(), "only works inside a Slack conversation") {
		t.Fatalf("a sign-in with no conversation was answered %v; it should have been offered to the owner", err)
	}
	if !strings.Contains(err.Error(), "could not be shown to anyone") {
		t.Fatalf("with no gateway attached the sign-in was answered %v", err)
	}
}
