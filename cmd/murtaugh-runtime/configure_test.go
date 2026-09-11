package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	configstore "github.com/miere/murtaugh/internal/config/store"
)

func nodeStore(t *testing.T) (config.Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := config.BootstrapNode(path); err != nil {
		t.Fatalf("BootstrapNode: %v", err)
	}
	_, store, err := configstore.BootstrapRole(context.Background(), path, config.RoleNode, true)
	if err != nil {
		t.Fatalf("BootstrapRole: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, dir
}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func profileBody(t *testing.T, value any) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

func storedWorkDir(t *testing.T, store config.Store, name string) string {
	t.Helper()
	body, ok, err := store.GetItem(context.Background(), config.SectionAgent, name)
	if err != nil || !ok {
		t.Fatalf("GetItem(%s): ok=%v err=%v", name, ok, err)
	}
	var row map[string]any
	if err := json.Unmarshal(body, &row); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	dir, _ := row["workdir"].(string)
	return dir
}

func TestApplyingAGatewaySuppliedConfigurationWritesThisNodesStore(t *testing.T) {
	store, dir := nodeStore(t)
	c := &configurer{store: store, baseDir: dir, logger: quiet(), restarts: true}

	general := config.AgentProfile{
		Native:  &config.NativeProfile{Provider: "anthropic", Model: "claude", APIKeyEnv: "ANTHROPIC_API_KEY"},
		WorkDir: "/home/dev/work",
	}
	tweaker := config.AgentProfile{
		Native: &config.NativeProfile{Provider: "anthropic", Model: "claude", APIKeyEnv: "ANTHROPIC_API_KEY"},
	}
	result, err := c.apply(context.Background(), agentwire.NodeConfiguration{
		Agents: map[string]json.RawMessage{
			"code":    profileBody(t, general),
			"tweaker": profileBody(t, tweaker),
		},
		Chat: profileBody(t, config.ChatConfig{
			Enabled:  true,
			Defaults: config.ChatDefaults{Agent: "code"},
		}),
		Env: map[string]string{"ANTHROPIC_API_KEY": "sk-test"},
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.Applied != 2 || !result.Restarting {
		t.Errorf("node reported %+v, want 2 applied and a restart", result)
	}

	cfg, err := store.Load(context.Background(), config.Config{Role: config.RoleNode, BaseDir: dir})
	if err != nil {
		t.Fatalf("the node's own configuration does not load after being configured: %v", err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("the node holds %d agent profiles, want 2", len(cfg.Agents))
	}
	if cfg.Chat.Defaults.Agent != "code" {
		t.Errorf("chat default is %q, want code", cfg.Chat.Defaults.Agent)
	}
	if got := storedWorkDir(t, store, "tweaker"); got != dir {
		t.Errorf("the tweaker is rooted at %q, want this node's config dir %q", got, dir)
	}
	if got := storedWorkDir(t, store, "code"); got != "/home/dev/work" {
		t.Errorf("the general profile's work_dir was overwritten: %q", got)
	}

	env, err := os.ReadFile(filepath.Join(dir, config.EnvFileName))
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	if !strings.Contains(string(env), "ANTHROPIC_API_KEY=sk-test") {
		t.Errorf("the provider credential did not reach this node's .env:\n%s", env)
	}
}

// Node admins own their node: a form left open on the gateway must not overwrite
// what the owner set from a terminal minutes ago.
func TestAConfiguredNodeRefusesToBeReconfiguredByItsGateway(t *testing.T) {
	store, dir := nodeStore(t)
	existing := config.AgentProfile{Native: &config.NativeProfile{Provider: "anthropic", Model: "claude", APIKeyEnv: "K"}}
	if err := store.UpsertItem(context.Background(), config.SectionAgent, "mine", existing); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}

	c := &configurer{store: store, baseDir: dir, logger: quiet(), restarts: true}
	_, err := c.apply(context.Background(), agentwire.NodeConfiguration{
		Agents: map[string]json.RawMessage{"code": profileBody(t, existing)},
	})
	if err == nil {
		t.Fatal("a configured node accepted a configuration from its gateway")
	}
	if !strings.Contains(err.Error(), "already") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	rows, err := store.ListItems(context.Background(), config.SectionAgent)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("the store holds %d agent profiles, want the 1 it already had", len(rows))
	}
}

// Reporting "saved nothing" as success would send the operator, who is watching
// Slack, off to debug a node that is fine.
func TestAnEmptyConfigurationIsRefused(t *testing.T) {
	store, dir := nodeStore(t)
	c := &configurer{store: store, baseDir: dir, logger: quiet()}
	if _, err := c.apply(context.Background(), agentwire.NodeConfiguration{}); err == nil {
		t.Fatal("an empty configuration was accepted")
	}
}

// A Slack token placeholder would invite people to fill it in on every node, and
// a node must never hold that token.
func TestANodesBootstrapFileOffersNoSlackCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := config.BootstrapNode(path); err != nil {
		t.Fatalf("BootstrapNode: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, forbidden := range []string{"oauth", "SLACK_APP_TOKEN", "SLACK_BOT_TOKEN"} {
			if strings.Contains(line, forbidden) {
				t.Errorf("a node's bootstrap file declares %q: %s", forbidden, line)
			}
		}
	}
	boot, err := config.LoadBootstrap(path)
	if err != nil {
		t.Fatalf("a node's seeded bootstrap file does not parse: %v", err)
	}
	if boot.OAuth != (config.OAuthConfig{}) {
		t.Errorf("a node's bootstrap file carries Slack credentials: %+v", boot.OAuth)
	}
	if boot.Database.IsZero() {
		t.Error("a node's bootstrap file selects no config-store backend")
	}
}
