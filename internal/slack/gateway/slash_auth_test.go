package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/credwarden"
)

func TestIsAuthSlashCommand(t *testing.T) {
	for text, want := range map[string]bool{
		"auth":                true,
		"auth status":         true,
		"AUTH":                true,
		"  auth  ":            true,
		"authenticate":        false,
		"restart":             false,
		"":                    false,
		"chat auth something": false,
	} {
		if got := isAuthSlashCommand(text); got != want {
			t.Errorf("isAuthSlashCommand(%q) = %v, want %v", text, got, want)
		}
	}
}

// Anything that is not an explicit `status` is the re-authentication request, so
// a typo cannot silently do nothing.
func TestAuthSlashWantsStatus(t *testing.T) {
	for text, want := range map[string]bool{
		"auth status": true,
		"auth STATUS": true,
		"auth":        false,
		"auth stauts": false,
		"auth login":  false,
	} {
		if got := authSlashWantsStatus(text); got != want {
			t.Errorf("authSlashWantsStatus(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestCredentialStatusWithoutClaudeCodeAgent(t *testing.T) {
	g := &Gateway{}
	got := g.credentialStatusText()
	if !strings.Contains(got, "No `claude_code` agent") {
		t.Fatalf("expected a clear no-agent message, got %q", got)
	}
}

func TestCredentialStatusReportsExpiryAndRefresh(t *testing.T) {
	w := credwarden.New(credwarden.Options{
		Identities: []credwarden.Identity{{Command: "/usr/local/bin/claude"}},
	})
	g := &Gateway{credWarden: w}

	got := g.credentialStatusText()
	if !strings.Contains(got, "/usr/local/bin/claude") {
		t.Fatalf("expected the credential to be named, got %q", got)
	}
	// Nothing observed yet: must say so rather than render a zero time as 1970.
	if !strings.Contains(got, "not yet read") {
		t.Fatalf("expected an unread expiry to be reported as such, got %q", got)
	}
	if !strings.Contains(got, "none this run") {
		t.Fatalf("expected the no-refresh case to be reported, got %q", got)
	}
}

// The status surface is rendered into Slack and the diagnostics bundle, so it
// must never carry credential material — only timings and errors.
func TestCredentialStatusCarriesNoSecretMaterial(t *testing.T) {
	w := credwarden.New(credwarden.Options{
		Identities: []credwarden.Identity{{Command: "/usr/local/bin/claude", Home: "/srv/x"}},
	})
	got := (&Gateway{credWarden: w}).credentialStatusText()
	for _, forbidden := range []string{"sk-ant", "accessToken", "refreshToken"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("status text leaked %q: %s", forbidden, got)
		}
	}
}

// A never-observed credential must not render its zero time as an epoch date.
func TestCredentialStatusNeverRendersEpochForUnreadExpiry(t *testing.T) {
	w := credwarden.New(credwarden.Options{
		Identities: []credwarden.Identity{{Command: "/bin/claude"}},
	})
	if got := (&Gateway{credWarden: w}).credentialStatusText(); strings.Contains(got, "1970") {
		t.Fatalf("zero expiry rendered as an epoch date: %s", got)
	}
}

func TestAuthVerbIsAdvertisedInHelp(t *testing.T) {
	resp := NewDefaultSlashCommandHandler().help("/murtaugh")

	var body strings.Builder
	body.WriteString(resp.Text)
	for _, block := range resp.Blocks {
		if section, ok := block.(*slack.SectionBlock); ok && section.Text != nil {
			body.WriteString(" " + section.Text.Text)
		}
	}
	if !strings.Contains(body.String(), "/murtaugh auth") {
		t.Fatalf("help text does not advertise the auth verb: %q", body.String())
	}
}

type pinnedFleet struct {
	pins      map[agent.ConversationKey]agentruntime.NodeRef
	connected []agentruntime.NodeRef
	status    agentruntime.RenewalStatus
	renewed   []string
}

func (f *pinnedFleet) pinned(_ context.Context, conversation agent.ConversationKey) (agentruntime.NodeRef, error) {
	if node, ok := f.pins[conversation]; ok {
		return node, nil
	}
	return agentruntime.NodeRef{}, agentruntime.ErrNotPinned
}

func (f *pinnedFleet) nodes() []agentruntime.NodeRef { return f.connected }

func (f *pinnedFleet) renew(_ context.Context, nodeID string) (agentruntime.RenewalStatus, error) {
	f.renewed = append(f.renewed, nodeID)
	return f.status, nil
}

func fleetGateway(fleet *pinnedFleet) *Gateway {
	return &Gateway{
		pinnedNode:      fleet.pinned,
		connectedNodes:  fleet.nodes,
		renewCredential: fleet.renew,
		cfg:             config.AccessConfig{AdminUser: "UADMIN00", AllowedUsers: []string{"UOWNER00", "UOTHER00", "USTRANGER"}},
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func testFleet(status agentruntime.RenewalStatus) *pinnedFleet {
	return &pinnedFleet{
		status: status,
		pins: map[agent.ConversationKey]agentruntime.NodeRef{
			{TeamID: "T1", ChannelID: "C1", ThreadTS: "171.1"}: {NodeID: "node-2", Owner: "UOWNER00"},
			{TeamID: "T1", ChannelID: "C2"}:                    {NodeID: "node-1", Owner: "UOTHER00"},
		},
		connected: []agentruntime.NodeRef{{NodeID: "node-1", Owner: "UOTHER00"}, {NodeID: "node-2", Owner: "UOWNER00"}},
	}
}

func TestAuthLoginSignsInTheNodeTheThreadIsPinnedTo(t *testing.T) {
	fleet := testFleet(agentruntime.RenewalStarted)
	g := fleetGateway(fleet)

	said := g.renewNodeCredential(slack.SlashCommand{TeamID: "T1", ChannelID: "C1", UserID: "UADMIN00", Text: "auth login"}, "171.1")
	if len(fleet.renewed) != 1 || fleet.renewed[0] != "node-2" {
		t.Fatalf("signed in %v, want only node-2, which the thread is pinned to", fleet.renewed)
	}
	if !strings.Contains(said.Text, "node-2") || !strings.Contains(said.Text, "<@UOWNER00>") {
		t.Fatalf("the admin was told %q", said.Text)
	}
	if said := g.renewNodeCredential(slack.SlashCommand{TeamID: "T1", ChannelID: "C2", UserID: "UOTHER00", Text: "auth login"}, ""); len(fleet.renewed) != 2 || fleet.renewed[1] != "node-1" {
		t.Fatalf("a channel pinned without a thread was answered %q", said.Text)
	}
	if said := g.renewNodeCredential(slack.SlashCommand{TeamID: "T1", ChannelID: "C1", UserID: "USTRANGER"}, "171.1"); len(fleet.renewed) != 2 || !strings.Contains(said.Text, "Only the admin") {
		t.Fatalf("someone who neither administers the gateway nor owns the node was answered %q", said.Text)
	}
	if said := g.renewNodeCredential(slack.SlashCommand{TeamID: "T1", ChannelID: "C1", UserID: "UADMIN00", Text: "auth login node-1"}, "171.1"); len(fleet.renewed) != 2 || !strings.Contains(said.Text, "runs on node `node-2`") {
		t.Fatalf("naming another node in a pinned thread was answered %q", said.Text)
	}
}

func TestAuthLoginWithNoPinRefusesAndListsTheNodesYouMayName(t *testing.T) {
	fleet := testFleet(agentruntime.RenewalStarted)
	said := fleetGateway(fleet).renewNodeCredential(slack.SlashCommand{TeamID: "T1", ChannelID: "C9", UserID: "UOWNER00", Text: "auth login"}, "")
	if len(fleet.renewed) != 0 {
		t.Fatalf("a conversation with no pin signed in %v", fleet.renewed)
	}
	if !strings.Contains(said.Text, "No runtime node is pinned") || !strings.Contains(said.Text, "`node-2`") || strings.Contains(said.Text, "`node-1`") {
		t.Fatalf("the owner was told %q", said.Text)
	}
}

func TestAuthLoginWithNoPinSignsInANodeYouName(t *testing.T) {
	fleet := testFleet(agentruntime.RenewalStarted)
	g := fleetGateway(fleet)
	if said := g.renewNodeCredential(slack.SlashCommand{TeamID: "T1", ChannelID: "C9", UserID: "UOWNER00", Text: "auth login node-2"}, ""); len(fleet.renewed) != 1 || fleet.renewed[0] != "node-2" {
		t.Fatalf("the owner naming their own node was answered %q", said.Text)
	}
	if said := g.renewNodeCredential(slack.SlashCommand{TeamID: "T1", ChannelID: "C9", UserID: "UOWNER00", Text: "auth login node-1"}, ""); len(fleet.renewed) != 1 || !strings.Contains(said.Text, "not yours") {
		t.Fatalf("naming someone else's node was answered %q", said.Text)
	}
	if said := g.renewNodeCredential(slack.SlashCommand{TeamID: "T1", ChannelID: "C9", UserID: "UOWNER00", Text: "auth login node-9"}, ""); len(fleet.renewed) != 1 || !strings.Contains(said.Text, "is connected") {
		t.Fatalf("naming a node that is not connected was answered %q", said.Text)
	}
	if said := g.renewNodeCredential(slack.SlashCommand{TeamID: "T1", ChannelID: "C9", UserID: "UADMIN00", Text: "auth login node-1"}, ""); len(fleet.renewed) != 2 || fleet.renewed[1] != "node-1" {
		t.Fatalf("the admin naming a connected node was answered %q", said.Text)
	}
}

func TestAuthLoginRefusesANodeWhoseOwnerLostAccess(t *testing.T) {
	fleet := testFleet(agentruntime.RenewalStarted)
	g := fleetGateway(fleet)
	g.setAccess(func(access *config.AccessConfig) { access.AllowedUsers = []string{"UOTHER00"} })
	said := g.renewNodeCredential(slack.SlashCommand{TeamID: "T1", ChannelID: "C1", UserID: "UADMIN00", Text: "auth login"}, "171.1")
	if len(fleet.renewed) != 0 || !strings.Contains(said.Text, "may no longer use this gateway") {
		t.Fatalf("a node whose owner lost access was signed in %v, the admin told %q", fleet.renewed, said.Text)
	}
}

func TestAuthLoginSaysWhenAnEarlierSignInWouldNotStop(t *testing.T) {
	fleet := testFleet(agentruntime.RenewalAlreadyRunning)
	said := fleetGateway(fleet).renewNodeCredential(slack.SlashCommand{TeamID: "T1", ChannelID: "C1", UserID: "UADMIN00"}, "171.1")
	if !strings.Contains(said.Text, "would not stop") || strings.Contains(said.Text, "on its way") {
		t.Fatalf("the admin was told %q", said.Text)
	}
}

func TestAuthLoginTypedInAThreadUsesThatThreadsPin(t *testing.T) {
	fleet := testFleet(agentruntime.RenewalStarted)
	event := socketmode.Event{Type: socketmode.EventTypeSlashCommand, Request: &socketmode.Request{Payload: json.RawMessage(`{"thread_ts":"171.1"}`)}}
	fleetGateway(fleet).handleAuthSlashCommand(event, slack.SlashCommand{TeamID: "T1", ChannelID: "C1", UserID: "UADMIN00", Text: "auth login"})
	if len(fleet.renewed) != 1 || fleet.renewed[0] != "node-2" {
		t.Fatalf("auth login typed in a pinned thread signed in %v", fleet.renewed)
	}
}
