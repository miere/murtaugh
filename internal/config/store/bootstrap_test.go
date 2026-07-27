package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

// writeLegacyConfigDir creates a pre-database config dir: a config.yaml with no
// `database:` block (oauth + access + chat) plus agents.yaml/jobs.yaml siblings
// and a .env. It returns the config.yaml path.
func writeLegacyConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("config.yaml", `oauth:
  app_token: ${SLACK_APP_TOKEN}
  bot_token: ${SLACK_BOT_TOKEN}
access:
  admin_user: U123
chat:
  enabled: true
  defaults:
    agent: code
`)
	write(".env", "SLACK_APP_TOKEN=xapp-legacy\nSLACK_BOT_TOKEN=xoxb-legacy\n")
	write("agents.yaml", `agents:
  code:
    native:
      provider: gemini
      model: gemini-2.5-pro
      api_key_env: GEMINI_API_KEY
`)
	write("jobs.yaml", `jobs:
  nightly:
    command: echo
`)
	return filepath.Join(dir, "config.yaml")
}

func TestBootstrapMigratesLegacyYAML(t *testing.T) {
	// Isolate the default SQLite location to a temp dir (EffectiveSQLitePath
	// honours XDG_STATE_HOME) so the test never touches the real home.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	gatewayPath := writeLegacyConfigDir(t)
	dir := filepath.Dir(gatewayPath)

	cfg, s, err := Bootstrap(context.Background(), gatewayPath, false)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	defer s.Close()

	// The tree came across into the store and assembled correctly.
	if _, ok := cfg.Agents["code"]; !ok {
		t.Errorf("agent not migrated: %+v", cfg.Agents)
	}
	if !cfg.Chat.Enabled || cfg.Chat.Defaults.Agent != "code" {
		t.Errorf("chat not migrated: %+v", cfg.Chat)
	}
	if cfg.Access.AdminUser != "U123" {
		t.Errorf("access not migrated: %+v", cfg.Access)
	}
	if len(cfg.Jobs) != 1 {
		t.Errorf("job not migrated: %+v", cfg.Jobs)
	}
	if cfg.OAuth.AppToken != "xapp-legacy" {
		t.Errorf("oauth not carried through / .env not loaded: %q", cfg.OAuth.AppToken)
	}

	// config.yaml was slimmed to oauth + database (no access/chat), keeping
	// the ${VAR} reference rather than the expanded secret.
	raw, err := os.ReadFile(gatewayPath)
	if err != nil {
		t.Fatal(err)
	}
	txt := string(raw)
	if !strings.Contains(txt, "database:") || !strings.Contains(txt, "backend: sqlite") {
		t.Errorf("config.yaml missing database block:\n%s", txt)
	}
	if strings.Contains(txt, "chat:") || strings.Contains(txt, "access:") {
		t.Errorf("config.yaml still has access/chat:\n%s", txt)
	}
	if !strings.Contains(txt, "${SLACK_APP_TOKEN}") {
		t.Errorf("config.yaml lost the ${VAR} reference (leaked secret?):\n%s", txt)
	}

	// The config DB lands beside config.yaml (the config dir) by default, not in
	// the XDG state dir — the store travels with its config.
	if _, err := os.Stat(filepath.Join(dir, "config.db")); err != nil {
		t.Errorf("config.db not created beside config.yaml: %v", err)
	}

	// Siblings were archived (moved), not left in place or deleted.
	if _, err := os.Stat(filepath.Join(dir, "agents.yaml")); !os.IsNotExist(err) {
		t.Errorf("agents.yaml not archived")
	}
	archives, _ := filepath.Glob(filepath.Join(dir, "migrated-*", "agents.yaml"))
	if len(archives) != 1 {
		t.Errorf("expected agents.yaml in a migrated-* archive, got %v", archives)
	}
}

func TestBootstrapIsIdempotent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	gatewayPath := writeLegacyConfigDir(t)

	cfg1, s1, err := Bootstrap(context.Background(), gatewayPath, false)
	if err != nil {
		t.Fatalf("first Bootstrap: %v", err)
	}
	s1.Close()

	// Second run: config.yaml now has a database block, so migration is skipped
	// and the config loads straight from the store.
	cfg2, s2, err := Bootstrap(context.Background(), gatewayPath, false)
	if err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	defer s2.Close()

	if _, ok := cfg2.Agents["code"]; !ok {
		t.Errorf("agent missing on reload: %+v", cfg2.Agents)
	}
	if cfg1.Chat.Defaults.Agent != cfg2.Chat.Defaults.Agent {
		t.Errorf("chat differs across reload: %q vs %q", cfg1.Chat.Defaults.Agent, cfg2.Chat.Defaults.Agent)
	}
	// No second archive dir should appear.
	archives, _ := filepath.Glob(filepath.Join(filepath.Dir(gatewayPath), "migrated-*"))
	if len(archives) != 1 {
		t.Errorf("expected exactly one migrated-* dir, got %d", len(archives))
	}
}

func TestBootstrapSetupSkipsLoad(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	// A fresh bootstrap config.yaml (already has a database block, à la the seed).
	body := `oauth:
  app_token: ${SLACK_APP_TOKEN}
  bot_token: ${SLACK_BOT_TOKEN}
database:
  backend: sqlite
`
	gatewayPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(gatewayPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// setup=true opens the store without loading/validating (no agents yet).
	cfg, s, err := Bootstrap(context.Background(), gatewayPath, true)
	if err != nil {
		t.Fatalf("Bootstrap(setup): %v", err)
	}
	defer s.Close()
	if s.Backend() != config.BackendSQLite {
		t.Errorf("want sqlite backend, got %q", s.Backend())
	}
	if len(cfg.Agents) != 0 {
		t.Errorf("setup bootstrap should not load agents, got %+v", cfg.Agents)
	}
}
