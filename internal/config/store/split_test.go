package store

import (
	"context"
	"os"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

// combinedStore fills a store the way a single, pre-split installation is
// filled: agents and MCP servers and jobs alongside access, election, chat and
// the rules, all in one place.
func combinedStore(t *testing.T, s config.Store) {
	t.Helper()
	ctx := context.Background()
	put := func(section, name string, body any) {
		t.Helper()
		if err := s.UpsertItem(ctx, section, name, body); err != nil {
			t.Fatalf("UpsertItem(%s/%s): %v", section, name, err)
		}
	}
	single := func(key string, body any) {
		t.Helper()
		if err := s.PutSingleton(ctx, key, body); err != nil {
			t.Fatalf("PutSingleton(%s): %v", key, err)
		}
	}

	put(config.SectionAgent, "code", config.AgentProfile{
		Native: &config.NativeProfile{Provider: "anthropic", Model: "claude", APIKeyEnv: "ANTHROPIC_API_KEY"},
	})
	put(config.SectionAgent, "tweaker", config.AgentProfile{
		Native: &config.NativeProfile{Provider: "anthropic", Model: "claude", APIKeyEnv: "ANTHROPIC_API_KEY"},
	})
	put(config.SectionMCP, "github", config.MCPServerConfig{Command: "gh-mcp"})
	put(config.SectionJob, "nightly", config.JobProfile{
		Agent: "code", Prompt: "sweep", Schedule: "0 3 * * *",
	})
	single(config.SingletonChat, config.ChatConfig{
		Enabled: true,
		Defaults: config.ChatDefaults{
			Agent:    "code",
			DMAgents: map[string]string{"U1": "tweaker"},
		},
		Channels: config.ChannelRules{{Match: "nc-*", Agent: "code"}},
	})
	single(config.SingletonAccess, config.AccessConfig{AdminUser: "U1", AllowedUsers: []string{"U2"}})
	single(config.SingletonDefaults, config.RuntimeDefaults{})
	single(config.SingletonElection, config.ElectionConfig{})
	single(config.SingletonJournal, config.JournalConfig{})
	single(config.SingletonTroubleshoot, config.TroubleshootConfig{Providers: []string{"goose"}})
}

// #198's migration test: one combined configuration becomes two, each of which
// loads under its OWN role.
//
// That last clause is the whole point. A copy that produced two stores neither
// of which could be loaded by the process meant to load it would be a file
// operation dressed as a migration — and the two roles disagree about what
// valid means, which is exactly the change this item makes.
func TestSplitForNodeGivesEachHalfAConfigurationItsOwnRoleAccepts(t *testing.T) {
	ctx := context.Background()
	gatewayStore := openTestStore(t)
	nodeStore := openTestStore(t)
	combinedStore(t, gatewayStore)

	report, err := SplitForNode(ctx, gatewayStore, nodeStore)
	if err != nil {
		t.Fatalf("SplitForNode: %v", err)
	}

	// The node's half: profile bodies, MCP servers, jobs, and the two sections
	// both halves read.
	node, err := nodeStore.Load(ctx, config.Config{Role: config.RoleNode})
	if err != nil {
		t.Fatalf("the node configuration does not load as a node: %v", err)
	}
	if len(node.Agents) != 2 {
		t.Errorf("node holds %d agent profiles, want 2 (%v)", len(node.Agents), report.Copied)
	}
	if _, ok := node.MCPServers["github"]; !ok {
		t.Error("node did not receive the MCP servers")
	}
	if _, ok := node.Jobs["nightly"]; !ok {
		t.Error("node did not receive the scheduled jobs")
	}
	// Without chat the node claims nothing: internal/nodeclaim derives the whole
	// advertisement from chat.channels plus chat.defaults.agent, so a node
	// missing it attaches and is never delegated anything.
	if node.Chat.Defaults.Agent != "code" {
		t.Errorf("node chat default is %q, want %q", node.Chat.Defaults.Agent, "code")
	}
	if len(node.Chat.Channels) != 1 {
		t.Errorf("node holds %d channel rules, want 1", len(node.Chat.Channels))
	}

	// What must NOT cross. Access carries the allowlist and the admin; election
	// timings and the rules are gateway decisions a node admin must not own.
	if node.Access.AdminUser != "" || len(node.Access.AllowedUsers) != 0 {
		t.Errorf("access crossed to the node: %+v", node.Access)
	}
	if body, ok, err := nodeStore.GetSingleton(ctx, config.SingletonElection); err != nil {
		t.Fatalf("GetSingleton(election): %v", err)
	} else if ok && len(body) > 0 {
		t.Errorf("election timings crossed to the node: %s", body)
	}

	// The gateway keeps everything: this copies and deletes nothing, because
	// deleting its agents is precisely what would switch the in-process path off
	// — and that path is still the shipping default.
	gatewayBase := config.Config{Role: config.RoleGateway, OAuth: config.OAuthConfig{AppToken: "x", BotToken: "x"}}
	gw, err := gatewayStore.Load(ctx, gatewayBase)
	if err != nil {
		t.Fatalf("the gateway configuration does not load as a gateway: %v", err)
	}
	if len(gw.Agents) != 2 {
		t.Errorf("the split removed the gateway's agents; it holds %d, want 2", len(gw.Agents))
	}
	if gw.Access.AdminUser != "U1" {
		t.Errorf("gateway lost its access config: %+v", gw.Access)
	}
	if report.Kept[config.SingletonAccess] != 1 {
		t.Errorf("report does not say access stayed: %+v", report.Kept)
	}
}

// A node's configuration must load without the Slack credentials it will never
// hold, and a gateway's must still load once a name it uses can only be resolved
// on a machine that is not connected. Both are the role change, checked from the
// side an operator meets it on.
func TestSplitHalvesFailUnderTheOtherHalfsRole(t *testing.T) {
	ctx := context.Background()
	gatewayStore := openTestStore(t)
	nodeStore := openTestStore(t)
	combinedStore(t, gatewayStore)
	if _, err := SplitForNode(ctx, gatewayStore, nodeStore); err != nil {
		t.Fatalf("SplitForNode: %v", err)
	}

	// The node store under the COMBINED role, which still requires Slack
	// tokens: this is what a node would hit if the role did not exist.
	if _, err := nodeStore.Load(ctx, config.Config{}); err == nil {
		t.Error("the node store loaded without Slack credentials under the combined role; the role guard is not doing anything")
	}

	// A gateway whose profile bodies have gone: valid as a gateway, invalid as
	// anything that claims to hold bodies. Emptied here rather than by the split
	// itself, because the split deliberately does not delete.
	if _, err := gatewayStore.DeleteItem(ctx, config.SectionAgent, "code"); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	if _, err := gatewayStore.DeleteItem(ctx, config.SectionAgent, "tweaker"); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	base := config.Config{OAuth: config.OAuthConfig{AppToken: "x", BotToken: "x"}}
	if _, err := gatewayStore.Load(ctx, base); err == nil {
		t.Error("a bodyless configuration loaded under the combined role; the write-time check has been lost, not moved")
	}
	if _, err := gatewayStore.Load(ctx, config.Config{Role: config.RoleGateway, OAuth: base.OAuth}); err != nil {
		t.Errorf("a broker gateway cannot load its own configuration: %v", err)
	}
}

// The CONFIGURATION half of the enterprise case #198 names: a laptop node whose
// store is SQLite, split out of a gateway whose store is Firestore. The two
// stores are opened independently, the copy crosses backends, and each half is
// validated under its own role — no part of the split assumes one backend, and
// nothing about the node's configuration refers to the gateway's.
//
// What this does NOT do, stated because the difference is easy to lose: no node
// attaches and no gateway starts. It is not an end-to-end mixed-backend attach —
// and it cannot usefully be, because a backend is invisible on the wire. What
// crosses a node connection is the credential it presents and the advertisement
// it makes, neither of which is read from the config store; the attach itself is
// covered over the loopback rig in internal/nodehost. The part that genuinely
// does depend on two real backends is right here: SplitForNode's copy, and each
// half loading under its own role afterwards.
//
// Skipped without the emulator, like every other Firestore test here.
func TestSplitFromAFirestoreGatewayToASQLiteNode(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("set FIRESTORE_EMULATOR_HOST (e.g. via docker compose up -d) to run Firestore tests")
	}
	ctx := context.Background()
	gatewayStore := openFirestoreTestStore(t)
	nodeStore := openTestStore(t)
	combinedStore(t, gatewayStore)

	if _, err := SplitForNode(ctx, gatewayStore, nodeStore); err != nil {
		t.Fatalf("SplitForNode across backends: %v", err)
	}
	node, err := nodeStore.Load(ctx, config.Config{Role: config.RoleNode})
	if err != nil {
		t.Fatalf("the SQLite node configuration does not load: %v", err)
	}
	if len(node.Agents) != 2 {
		t.Errorf("the SQLite node holds %d agent profiles, want 2", len(node.Agents))
	}
	if node.Database.EffectiveBackend() == gatewayStore.Backend() {
		t.Errorf("this test is not exercising two backends: both are %q", gatewayStore.Backend())
	}
}

// Node token hashes and conversation pins live in side stores Snapshot
// deliberately excludes, so they cannot travel through the split. Copying the
// hashes would put the fleet's whole credential table on a laptop; copying pins
// would give a node an opinion about delegations that are not its business.
//
// Asserted rather than commented because the protection is indirect — it holds
// only for as long as Snapshot stays narrow, and somebody widening Snapshot for
// a good reason would break this silently.
func TestSplitCannotCarryNodeCredentialsOrPins(t *testing.T) {
	ctx := context.Background()
	gatewayStore := openTestStore(t)
	nodeStore := openTestStore(t)
	combinedStore(t, gatewayStore)
	if _, err := SplitForNode(ctx, gatewayStore, nodeStore); err != nil {
		t.Fatalf("SplitForNode: %v", err)
	}
	snap, err := nodeStore.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for _, item := range snap.Items {
		if !config.ValidSection(item.Section) {
			t.Errorf("the node store holds an unexpected section %q", item.Section)
		}
		switch item.Section {
		case config.SectionAgent, config.SectionMCP, config.SectionJob:
		default:
			t.Errorf("section %q crossed to the node and should not have", item.Section)
		}
	}
	for _, single := range snap.Singletons {
		switch single.Key {
		case config.SingletonChat, config.SingletonDefaults:
		default:
			t.Errorf("singleton %q crossed to the node and should not have", single.Key)
		}
	}
}
