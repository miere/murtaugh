package local

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/tools"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(discard{}, nil)) }

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func agentsConfig(t *testing.T) config.Config {
	t.Helper()
	return config.Config{
		BaseDir: t.TempDir(),
		Agents: map[string]config.AgentProfile{
			"default": {},
		},
	}
}

// `jobs.run` reports "agent delegation is unavailable" by nil-checking the
// delegator it was given. A typed nil — a (*agentdelegate.Runner)(nil) inside a
// non-nil interface — sails straight past that check and panics on the first
// delegated job instead.
//
// Returning the interface's zero value is the only thing that makes the check
// work, and it is invisible at the call site, so it is pinned here.
func TestDelegatorIsAGenuineNilWhenNoAgentIsConfigured(t *testing.T) {
	d := Delegator(config.Config{}, tools.NewRegistry())

	if d != nil {
		t.Fatalf("Delegator() = %#v, want a nil interface; a typed nil defeats every caller's nil check", d)
	}
}

func TestDelegatorIsBuiltWhenAnAgentIsConfigured(t *testing.T) {
	if d := Delegator(agentsConfig(t), tools.NewRegistry()); d == nil {
		t.Fatal("Delegator() = nil with an agent configured; every delegated job would report delegation unavailable")
	}
}

// Chat off does NOT mean "no agent machinery". A scheduled job, a workflow
// trigger and an unfurl all delegate with no thread in sight, and an
// acp/claude_code agent reaches Murtaugh's tools only through the aggregator —
// so both the delegator and the tool surface are built regardless. What chat
// gates is the session managers, which nothing would ever prompt.
func TestHeadlessRuntimeStillDelegatesAndStillServesTools(t *testing.T) {
	rt := Builder(agentsConfig(t), tools.NewRegistry(), quiet())(agentruntime.Hooks{Chat: false})

	if len(rt.Sessions) != 0 {
		t.Errorf("Sessions = %v with chat disabled, want none: nothing would ever prompt them", rt.Sessions)
	}
	if rt.Delegator == nil {
		t.Error("Delegator = nil with chat disabled; a scheduled agent job would report delegation unavailable")
	}
	if rt.ServeTools == nil {
		t.Error("ServeTools = nil with chat disabled; a delegated acp/claude_code agent could not post its own result")
	}
}

// No agents configured is the zero Runtime, and every consumer already handles
// it. Building an aggregator for nobody would bind a socket no agent will ever
// dial.
func TestRuntimeIsEmptyWhenNoAgentIsConfigured(t *testing.T) {
	rt := Builder(config.Config{}, tools.NewRegistry(), quiet())(agentruntime.Hooks{Chat: true})

	if rt.Sessions != nil || rt.Delegator != nil || rt.ServeTools != nil || rt.ToolProblems != nil {
		t.Fatalf("Runtime = %+v with no agents configured, want the zero value", rt)
	}
}

// The socket path carries the pid so two gateways on one machine — an old one
// standing down while a new one starts, say — never fight over one socket. It is
// also short, because unix socket paths are length-capped (~104 bytes on macOS)
// and a bind failure here costs every acp/claude_code agent its Murtaugh tools.
//
// Stable within a run, too: an agent is handed this path in its session env and
// looks for the aggregator there again after a demotion.
func TestSocketPathIsPerProcessShortAndStable(t *testing.T) {
	path := SocketPath()

	if !strings.Contains(path, strconv.Itoa(os.Getpid())) {
		t.Errorf("SocketPath() = %q does not carry pid %d; two gateways on one machine would collide", path, os.Getpid())
	}
	if len(path) > 100 {
		t.Errorf("SocketPath() = %q is %d bytes; a unix socket path this long will not bind", path, len(path))
	}
	if again := SocketPath(); again != path {
		t.Errorf("SocketPath() = %q then %q; an agent handed the first would not find the aggregator after a re-promotion", path, again)
	}
}
