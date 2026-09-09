package cfg

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

// `cfg …` writes are held to the rules of whichever half of #170's split this
// process is, and one rule differs: whether a name can be checked against a
// body here at all.
//
// The package variable is process-scoped and these tests set it, so each
// restores it — a leaked role would silently change the rules every other test
// in this package is written against.
func withRole(t *testing.T, r config.Role) {
	t.Helper()
	previous := role
	role = r
	t.Cleanup(func() { role = previous })
}

// A combined install must keep refusing a typo, which is the write-time check
// operators have today.
func TestACombinedInstallStillRefusesAnUnknownDefaultAgent(t *testing.T) {
	withRole(t, config.RoleCombined)
	p := testProvider(t)
	singles := SingletonTools(p)

	_, err := invoke(t, find(t, singles, "cfg.chat.set"), map[string]any{
		"enabled":       true,
		"default_agent": "code",
	})
	if err == nil {
		t.Fatal("a combined install accepted a default agent it holds no profile for")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("the refusal does not name the problem: %v", err)
	}
}

// A broker gateway must ACCEPT it. The body lives on somebody's node, this
// process holds none, and the check moved to connect time — held to the
// combined rules the gateway could never be configured at all.
//
// This is the behavioural change #198 asks to be documented rather than
// discovered, seen from the command an operator types.
func TestABrokerGatewayMayNameAProfileItDoesNotHold(t *testing.T) {
	withRole(t, config.RoleGateway)
	p := testProvider(t)
	singles := SingletonTools(p)

	if _, err := invoke(t, find(t, singles, "cfg.chat.set"), map[string]any{
		"enabled":       true,
		"default_agent": "code",
	}); err != nil {
		t.Fatalf("a broker gateway could not name the agent its fleet serves: %v", err)
	}

	// What did NOT move: a blank name needs no body to detect and is still
	// refused for every role.
	if _, err := invoke(t, find(t, singles, "cfg.chat.set"), map[string]any{
		"default_agent": "",
	}); err == nil {
		t.Error("a broker gateway accepted a blank chat.defaults.agent")
	}
}

// The node block is the one section whose contents are about the gateway and
// whose owner is the node. Its addresses are checked where they are configured,
// because the dialler's refusal arrives inside a redial loop designed to be
// patient.
func TestCfgNodeSetChecksTheSeedAddresses(t *testing.T) {
	withRole(t, config.RoleNode)
	p := testProvider(t)
	singles := SingletonTools(p)

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

// TestABrokerGatewayCanMigrateItsConfigStore is the same rule, at the one call
// site that did not get it.
//
// `cfg db migrate` validated the copy as RoleCombined, so on a broker gateway
// BOTH regimes ran: `chat.defaults.agent` was accepted at write time by the
// test above and then refused by the migration, every time. Moving the config
// store is the PREREQUISITE for the Firestore deployment — election needs a
// store every node can reach — so a gateway that cannot migrate is a gateway
// that cannot be deployed the way #198 exists for.
//
// The DSN is deliberately unusable: what is under test is that the command gets
// PAST its own configuration and as far as the target, not that Postgres works.
func TestABrokerGatewayCanMigrateItsConfigStore(t *testing.T) {
	withRole(t, config.RoleGateway)
	p := testProvider(t)

	if _, err := invoke(t, find(t, SingletonTools(p), "cfg.chat.set"), map[string]any{
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
		t.Fatalf("cfg db migrate holds a broker gateway to the combined rules, so it can never complete a migration: %v", err)
	}
}

// TestAnInvalidStoreIsRefusedBeforeAnythingIsCopied pins the ORDER.
//
// The check used to sit after Restore had copied the whole store into the
// target and before config.yaml was rewritten — so a refusal left a fully
// populated store that nothing points at, and running the command again made a
// second one. Validating the source first means a configuration that cannot
// survive its own rules never reaches a target at all.
//
// The target here cannot even be opened, which is what makes the assertion
// sharp: if the validation ran after the copy, this would fail with "open
// target store" and the real problem would never be named.
func TestAnInvalidStoreIsRefusedBeforeAnythingIsCopied(t *testing.T) {
	withRole(t, config.RoleCombined)
	p := testProvider(t)
	s, err := p()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	// Written past the cfg tools on purpose: they would refuse it, which is how
	// a store reaches this state at all — it was written by an older binary, or
	// by a role whose rules were different.
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
	// And the refusal names the role it was judged under, because that is the
	// one thing an operator cannot see from where they are standing: the same
	// store is valid for a broker gateway and invalid for a combined install,
	// and "not valid" without the half is a sentence they cannot act on. This is
	// config.Role.String's only caller — the zero Role is stored as "" and would
	// otherwise leave a hole in the middle of the message.
	if !strings.Contains(err.Error(), "not valid for a combined install") {
		t.Errorf("the refusal does not say which half judged it: %v", err)
	}
}
