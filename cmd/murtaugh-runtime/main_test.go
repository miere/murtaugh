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

// The report is this binary's whole output today, and its job is to answer one
// question before the node link exists: would this machine serve anything, and
// as what? A node enrolled against a gateway that then serves nothing is the
// failure it is here to make visible, so a configured agent must appear by name
// AND by resolved kind — the kind is what decides which backend the node would
// start, and it is defaulted rather than stored, so printing the raw field would
// show a blank for every agent that never set one.
func TestReportNamesEachAgentAndItsResolvedKind(t *testing.T) {
	cfg := config.Config{
		BaseDir: "/tmp/murtaugh-runtime-test",
		Agents: map[string]config.AgentProfile{
			// The kind is not stored: it is derived from which backend
			// sub-block is present, and absent means native.
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
	// Sorted, because this is read by a human comparing two machines and map
	// order would make that a diff of nothing.
	if strings.Index(got, "default (") > strings.Index(got, "zeta (") {
		t.Errorf("agents are not listed in name order; got:\n%s", got)
	}
}

// An unconfigured machine must say so rather than printing an empty list that
// reads like a successful enrolment.
func TestReportSaysSoWhenNoAgentIsConfigured(t *testing.T) {
	var out strings.Builder
	reportAgents(&out, config.Config{BaseDir: "/tmp/murtaugh-runtime-test"}, nil)

	if got := out.String(); !strings.Contains(got, "agents: none configured") {
		t.Errorf("report does not say the machine has no agents; got:\n%s", got)
	}
}

// Both hooks a node contributes have to be set, and neither failure is
// visible: an agent built with no Approver runs side-effecting tools
// unprompted, and one built with no BackgroundEvents has claude_code drop a
// background stretch's events at the backend — so the gateway's "went quiet"
// notice never appears, with no error anywhere near the gateway.
//
// The identity check is the point rather than a nil check: a hook wired to
// something other than the connection-bound sink would typecheck, pass a nil
// check, and still never reach the link.
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

// A node with nowhere to dial must FAIL rather than idle: a supervisor pointed
// at this binary before it was told where its gateway is has to see a non-zero
// exit, not a process that starts cleanly and serves nothing.
//
// It drives run() rather than reading the sentinel, because the sentinel is
// worth nothing if some later edit forgets to return it. Everything before that
// point is real — migration, config bootstrap, opening the store — so this also
// pins that the startup itself works against a fresh config directory.
func TestRunRefusesToStartWithNoGatewayToDial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	err := run([]string{"--config", path})

	if !errors.Is(err, errNoGateway) {
		t.Fatalf("run() = %v, want errNoGateway — the binary must not exit 0 having served nothing", err)
	}
	// The direction is the part an operator gets wrong, so the error says it.
	if !strings.Contains(err.Error(), "a node dials in") {
		t.Errorf("run() = %q, want it to say which end dials", err)
	}
}

// The bridge subcommand has to exist on THIS binary, not only on `murtaugh`.
//
// An acp or claude_code agent reaches Murtaugh's tools through a subprocess the
// aggregator advertises as os.Executable() with argv `mcp-bridge` — and on a
// node that executable is murtaugh-runtime. Without this branch the flag parser
// rejects the positional argument, the agent spawns a process that exits
// instantly, every session, and the only symptom is an agent with no Murtaugh
// tools and nothing in any log naming the cause.
//
// The assertion is on the error, because reaching the environment check proves
// the dispatch happened: the flag parser's refusal has different words.
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

// The node's tools run here and nothing of the gateway's is offered, so `help`
// describes exactly what this node's agent can call.
func TestANodeServesItsOwnTools(t *testing.T) {
	registry := nodeTools()
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
