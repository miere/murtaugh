package nodehost_test

import (
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/nodehost"
)

func TestANodesCredentialReportsReachTheGatewayAsItsOwners(t *testing.T) {
	expiry := time.Now().Add(6 * time.Hour).Truncate(time.Second)
	heard := make(chan agentruntime.CredentialHealth, 4)
	rig := dialLoopback(t, newScriptedAgent(func(*scriptedTurn) {}), reportingCredentials(
		func() []agentwire.CredentialHealth {
			return []agentwire.CredentialHealth{{Credential: "/opt/claude", ExpiresAt: expiry}}
		},
		func(h agentruntime.CredentialHealth) { heard <- h },
	))

	first := awaitHealth(t, heard)
	if first.NodeID != "node-1" || first.Owner != nodeOwner || first.Credential != "/opt/claude" || first.Degraded || !first.ExpiresAt.Equal(expiry) {
		t.Fatalf("the connect-time report arrived as %+v", first)
	}

	since := time.Now().Add(-time.Minute).Truncate(time.Second)
	rig.credentials.Report(agentwire.CredentialHealth{Credential: "/opt/claude", Degraded: true, Reason: "force refresh: exit status 1", Since: since})
	failing := awaitHealth(t, heard)
	if !failing.Degraded || failing.Owner != nodeOwner || failing.Reason != "force refresh: exit status 1" || !failing.Since.Equal(since) {
		t.Fatalf("the failure arrived as %+v", failing)
	}
	if got := rig.reports(); len(got) != 1 || !got[0].Degraded || got[0].NodeID != "node-1" {
		t.Fatalf("the gateway's status holds %+v, want the node's last word", got)
	}

	rig.host.DetachAll("the test dropped the link")
	waitFor(t, "the detached node's reports to be forgotten", func() bool { return len(rig.reports()) == 0 })
}

func awaitHealth(t *testing.T, heard <-chan agentruntime.CredentialHealth) agentruntime.CredentialHealth {
	t.Helper()
	select {
	case h := <-heard:
		return h
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway never heard the node's credential report")
		return agentruntime.CredentialHealth{}
	}
}

func TestARuntimeOverNodesDoesNotRunAgentsInProcess(t *testing.T) {
	host, err := nodehost.New(nodehost.Options{Tokens: &memTokens{records: map[string]config.NodeToken{}}, Logger: testLogger()})
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	rt := nodehost.Runtime(host)(config.Config{Agents: map[string]config.AgentProfile{"default": {ClaudeCode: &config.ClaudeCodeProfile{Command: "claude"}}}}, testLogger())(agentruntime.Hooks{Chat: true})
	if rt.InProcess {
		t.Fatal("a runtime whose agents run on nodes claims to run them in this process, so the gateway would keep their credentials itself")
	}
}
