package app

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentdelegate"
	"github.com/miere/murtaugh/internal/agentruntime/local"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/journal"
	"github.com/miere/murtaugh/internal/tools"
)

// completingClient is an agent.Client that accepts one prompt and immediately
// completes the turn, so a delegated job run finishes without a real backend.
type completingClient struct {
	prompts int
	reply   string
}

func (c *completingClient) Initialize(context.Context) error { return nil }

func (c *completingClient) NewSession(context.Context, agent.SessionMetadata) (agent.Session, error) {
	return agent.Session{ID: "session-1"}, nil
}

func (c *completingClient) Prompt(context.Context, string, agent.PromptRequest) (<-chan agent.Event, error) {
	c.prompts++
	ch := make(chan agent.Event, 2)
	if c.reply != "" {
		ch <- agent.Event{Type: agent.EventText, Text: c.reply}
	}
	ch <- agent.Event{Type: agent.EventComplete}
	close(ch)
	return ch, nil
}

func (c *completingClient) Cancel(context.Context, string) error { return nil }

func (c *completingClient) Close() error { return nil }

func agentJobConfig() config.Config {
	return config.Config{
		Agents: map[string]config.AgentProfile{
			"default": {ACP: &config.ACPProfile{Command: "/bin/true"}},
		},
		Jobs: map[string]config.JobProfile{
			"digest": {Agent: "default", Prompt: "summarise yesterday and post it"},
		},
	}
}

func testApp() *Application {
	return &Application{
		recorder: journal.NopRecorder{},
		registry: tools.NewRegistry(),
		agents:   Agents{Runtime: local.Builder, Delegator: local.Delegator},
	}
}

// A scheduled agent job must run through the gateway's own delegate runner —
// the one holding the MCP aggregator — not a second one built without it. If
// the scheduler ever builds its own again, the job still "succeeds" while the
// agent quietly has no tools to post with, so assert on the runner identity.
func TestScheduledRunnerUsesTheGatewayDelegator(t *testing.T) {
	client := &completingClient{}
	used := false
	gatewayRunner := agentdelegate.NewRunner(agentJobConfig().Agents, config.RuntimeDefaults{}, t.TempDir(), slog.Default()).
		WithClientFactory(func(config.AgentProfile, *slog.Logger) agent.Client {
			used = true
			return client
		})

	exec := testApp().newScheduledRunner(agentJobConfig(), gatewayRunner)
	if _, err := exec(context.Background(), "digest"); err != nil {
		t.Fatalf("scheduled run failed: %v", err)
	}
	if !used {
		t.Fatal("the scheduler built its own delegator instead of using the gateway's bridged one")
	}
	if client.prompts != 1 {
		t.Fatalf("agent prompted %d times, want 1", client.prompts)
	}
}

func TestScheduledRunnerHandsTheReplyToTheGateway(t *testing.T) {
	client := &completingClient{reply: "backups are green"}
	gatewayRunner := agentdelegate.NewRunner(agentJobConfig().Agents, config.RuntimeDefaults{}, t.TempDir(), slog.Default()).
		WithClientFactory(func(config.AgentProfile, *slog.Logger) agent.Client { return client })

	reply, err := testApp().newScheduledRunner(agentJobConfig(), gatewayRunner)(context.Background(), "digest")
	if err != nil {
		t.Fatalf("scheduled run failed: %v", err)
	}
	if reply == nil || reply.Text != "backups are green" || !reply.InProcess {
		t.Fatalf("reply = %+v, want the agent's text marked as run in process", reply)
	}
}

// With no agents configured the gateway has no runner to lend. The nil must
// stay a nil interface inside jobs.run — a typed nil pointer would satisfy the
// interface and panic on the first call instead of reporting the misconfig.
func TestScheduledRunnerWithoutAgentsReportsTheMisconfiguration(t *testing.T) {
	cfg := agentJobConfig()
	cfg.Agents = nil

	exec := testApp().newScheduledRunner(cfg, nil)
	_, err := exec(context.Background(), "digest")
	if err == nil {
		t.Fatal("expected an error for an agent job with no agents configured")
	}
	if !strings.Contains(err.Error(), "agent delegation is unavailable") {
		t.Fatalf("unexpected error: %v", err)
	}
}
