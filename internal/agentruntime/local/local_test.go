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

// A typed nil would slip past jobs.run's nil check and break the first delegated
// job, and nothing at the call site shows the difference.
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

// Chat off does not mean no agents: jobs, workflow triggers and unfurls still
// delegate, and acp/claude_code agents reach tools only through the aggregator.
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

func TestRuntimeIsEmptyWhenNoAgentIsConfigured(t *testing.T) {
	rt := Builder(config.Config{}, tools.NewRegistry(), quiet())(agentruntime.Hooks{Chat: true})

	if rt.Sessions != nil || rt.Delegator != nil || rt.ServeTools != nil || rt.ToolProblems != nil {
		t.Fatalf("Runtime = %+v with no agents configured, want the zero value", rt)
	}
}

// It must be stable within a run: an agent gets this path once, in its session
// env, and looks for the aggregator there again after a demotion.
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
