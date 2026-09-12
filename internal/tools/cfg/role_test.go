package cfg

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

// A node holds the profile bodies, so the write-time check it always had must
// still fire there — the deferral is the gateway's, not everybody's.
func TestANodeStillRefusesAnUnknownDefaultAgent(t *testing.T) {
	p := testProvider(t, config.RoleNode)

	_, err := invoke(t, find(t, NodeSingletonTools(p), "cfg.chat.set"), map[string]any{
		"enabled":       true,
		"default_agent": "code",
	})
	if err == nil {
		t.Fatal("a node accepted a default agent it holds no profile for")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("the refusal does not name the problem: %v", err)
	}
}

// The profile's body lives on somebody's node and is checked at connect time; held to
// the node's rules, a broker gateway could never be configured at all.
func TestABrokerGatewayMayNameAProfileItDoesNotHold(t *testing.T) {
	p := testProvider(t, config.RoleGateway)
	singles := GatewaySingletonTools(p)

	if _, err := invoke(t, find(t, singles, "cfg.chat.set"), map[string]any{
		"enabled":       true,
		"default_agent": "code",
	}); err != nil {
		t.Fatalf("a broker gateway could not name the agent its fleet serves: %v", err)
	}

	if _, err := invoke(t, find(t, singles, "cfg.chat.set"), map[string]any{
		"default_agent": "",
	}); err == nil {
		t.Error("a broker gateway accepted a blank chat.defaults.agent")
	}
}

// Checked here because a bad address would otherwise surface only inside the node's
// redial loop, which is built to keep retrying quietly.
func TestCfgNodeSetChecksTheSeedAddresses(t *testing.T) {
	p := testProvider(t, config.RoleNode)
	singles := NodeSingletonTools(p)

	if _, err := invoke(t, find(t, singles, "cfg.node.set"), map[string]any{
		"gateway": []any{"https://gateway.example.com"},
	}); err == nil {
		t.Fatal("an https:// address was accepted as a gateway seed")
	}
	if _, err := invoke(t, find(t, singles, "cfg.node.set"), map[string]any{
		"gateway": []any{"wss://gateway.example.com:8443", "ws://127.0.0.1:8787"},
	}); err != nil {
		t.Fatalf("valid seed addresses were refused: %v", err)
	}

	show, err := invoke(t, find(t, singles, "cfg.node.show"), nil)
	if err != nil {
		t.Fatalf("cfg.node.show: %v", err)
	}
	body, ok := show.(showResult)
	if !ok {
		t.Fatalf("cfg.node.show returned %T, want a stored body", show)
	}
	if !strings.Contains(string(body.Body), "gateway.example.com") {
		t.Errorf("the stored node config does not carry the seed: %s", body.Body)
	}
}

// The DSN is unusable on purpose: the test only needs the command to get past its own
// validation to the target, not a working Postgres.
func TestABrokerGatewayCanMigrateItsConfigStore(t *testing.T) {
	p := testProvider(t, config.RoleGateway)

	if _, err := invoke(t, find(t, GatewaySingletonTools(p), "cfg.chat.set"), map[string]any{
		"enabled":       true,
		"default_agent": "code",
	}); err != nil {
		t.Fatalf("a broker gateway could not name the agent its fleet serves: %v", err)
	}

	t.Setenv("MURTAUGH_TEST_MIGRATE_DSN", "://not-a-dsn")
	migrate := find(t, DBTools(p, filepath.Join(t.TempDir(), "config.yaml")), "cfg.db.migrate")
	_, err := invoke(t, migrate, map[string]any{
		"to":      config.BackendPostgres,
		"dsn_env": "MURTAUGH_TEST_MIGRATE_DSN",
	})
	if err == nil {
		t.Fatal("an unusable DSN was accepted")
	}
	if strings.Contains(err.Error(), "agents") {
		t.Fatalf("cfg db migrate holds a broker gateway to the node's rules, so it can never complete a migration: %v", err)
	}
}

// A refusal after the copy used to leave a full store nothing points at; the target
// here can't even be opened, so a check that ran late would fail with the wrong error.
func TestAnInvalidStoreIsRefusedBeforeAnythingIsCopied(t *testing.T) {
	p := testProvider(t, config.RoleNode)
	s, err := p.Store()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := s.PutSingleton(context.Background(), config.SingletonChat,
		config.ChatConfig{Enabled: true, Defaults: config.ChatDefaults{Agent: "gone"}}); err != nil {
		t.Fatalf("seed an invalid store: %v", err)
	}

	t.Setenv("MURTAUGH_TEST_MIGRATE_DSN", "://not-a-dsn")
	migrate := find(t, DBTools(p, filepath.Join(t.TempDir(), "config.yaml")), "cfg.db.migrate")
	_, err = invoke(t, migrate, map[string]any{
		"to":      config.BackendPostgres,
		"dsn_env": "MURTAUGH_TEST_MIGRATE_DSN",
	})
	if err == nil {
		t.Fatal("an invalid config store was migrated")
	}
	if !strings.Contains(err.Error(), "only move the problem") {
		t.Fatalf("the invalid store was not refused before the copy; the failure came from further down the command: %v", err)
	}
	if !strings.Contains(err.Error(), "not valid for a node") {
		t.Errorf("the refusal does not say which half judged it: %v", err)
	}
}
