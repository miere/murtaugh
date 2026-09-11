package gateway

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/credwarden"
)

func TestStartBackgroundIsNoOpWithoutAWarden(t *testing.T) {
	g := &Gateway{}
	g.StartBackground(context.Background()) // no claude_code agent: nothing to run
	g.StopBackground()                      // and stopping what never started is safe
}

// A configuration reload builds a replacement gateway. If the outgoing one kept
// its warden, two would run against the same credential and race the server's
// rotation of the refresh token — the failure the warden exists to prevent.
func TestStopBackgroundIsIdempotent(t *testing.T) {
	g := &Gateway{credWarden: credwarden.New(credwarden.Options{
		Identities: []credwarden.Identity{{Command: "/bin/claude"}},
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g.StartBackground(ctx)
	g.StartBackground(ctx) // second call must not start a second warden
	g.StopBackground()
	g.StopBackground() // and stopping twice must not panic
}

func gatewayOver(t *testing.T, runtime agentruntime.Runtime) (*Gateway, agentruntime.Hooks) {
	t.Helper()
	var hooks agentruntime.Hooks
	g := New(config.Config{
		OAuth:  config.OAuthConfig{AppToken: "xapp-test", BotToken: "xoxb-test"},
		Access: config.AccessConfig{AdminUser: "UADMIN", AllowedUsers: []string{"UOWNER"}},
		Agents: map[string]config.AgentProfile{"claude": {ClaudeCode: &config.ClaudeCodeProfile{Command: "/usr/local/bin/claude"}}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil, nil, func(h agentruntime.Hooks) agentruntime.Runtime {
		hooks = h
		return runtime
	})
	return g, hooks
}

func TestAGatewayWhoseAgentsRunOnNodesWatchesNoCredential(t *testing.T) {
	if g, _ := gatewayOver(t, agentruntime.Runtime{}); g.credWarden != nil {
		t.Fatalf("a gateway with no agents of its own watches %v", g.credWarden.Identities())
	}
	if g, _ := gatewayOver(t, agentruntime.Runtime{InProcess: true}); g.credWarden == nil {
		t.Fatal("a gateway running its own claude_code agent does not keep its credential fresh")
	}
}

func TestANodesCredentialReportReachesItsOwnerAndTheStatus(t *testing.T) {
	reports := []agentruntime.CredentialHealth{nodeReport(true)}
	g, hooks := gatewayOver(t, agentruntime.Runtime{CredentialReports: func() []agentruntime.CredentialHealth { return reports }})
	sink := &cardSink{}
	g.credAlerts = testAlerter(t, sink, "UADMIN")
	if hooks.CredentialHealth == nil {
		t.Fatal("the gateway gave the runtime nowhere to report a node's credentials")
	}
	hooks.CredentialHealth(nodeReport(true))
	posts, _ := sink.snapshot()
	if len(posts) != 1 || posts[0].ChannelID != "D-UOWNER" {
		var channels []string
		for _, p := range posts {
			channels = append(channels, p.ChannelID)
		}
		t.Fatalf("posted to %v, want one card in the node owner's DM", channels)
	}
	status := g.credentialStatusText()
	for _, want := range []string{"node-1", "<@UOWNER>", "/usr/local/bin/claude", "failing", "no expiresAt"} {
		if !strings.Contains(status, want) {
			t.Errorf("status does not show %q:\n%s", want, status)
		}
	}
}
