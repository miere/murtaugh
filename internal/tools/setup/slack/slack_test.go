package slack

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/store"
)

func newStore(t *testing.T) (config.Store, StoreProvider) {
	t.Helper()
	dbc := config.DatabaseConfig{
		Backend: config.BackendSQLite,
		SQLite:  config.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")},
	}
	s, err := store.Open(context.Background(), dbc, "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, func() (config.Store, error) { return s, nil }
}

func validArgs() map[string]any {
	return map[string]any{
		"app_token":  "xapp-1-test",
		"bot_token":  "xoxb-test",
		"admin_user": "@admin",
	}
}

func gateway(t *testing.T, path string) config.Config {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return cfg
}

func TestTool_Metadata(t *testing.T) {
	_, prov := newStore(t)
	tl := New(func() string { return "" }, prov)
	if tl.Name() != "setup.slack" {
		t.Fatalf("Name() = %q, want setup.slack", tl.Name())
	}
	required := map[string]bool{}
	for _, r := range tl.InputSchema().Required {
		required[r] = true
	}
	for _, want := range []string{"app_token", "bot_token", "admin_user"} {
		if !required[want] {
			t.Fatalf("required missing %q", want)
		}
	}
}

func TestInvoke_RejectsBadInputs(t *testing.T) {
	_, prov := newStore(t)
	tl := New(func() string { return filepath.Join(t.TempDir(), "gateway.yaml") }, prov)
	cases := []map[string]any{
		{},
		{"app_token": "no-prefix", "bot_token": "xoxb-x", "admin_user": "@a"},
		{"app_token": "xapp-x", "bot_token": "no-prefix", "admin_user": "@a"},
		{"app_token": "xapp-x", "bot_token": "xoxb-x", "admin_user": ""},
	}
	for i, args := range cases {
		if _, err := tl.Invoke(context.Background(), args); err == nil {
			t.Fatalf("case %d: Invoke returned nil, want error for %+v", i, args)
		}
	}
}

func TestInvoke_WritesOAuthAndStoresAccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	s, prov := newStore(t)
	tl := New(func() string { return path }, prov)

	res, err := tl.Invoke(context.Background(), validArgs())
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)

	// gateway.yaml references the tokens, never embeds them.
	cfg := gateway(t, path)
	if cfg.OAuth.AppToken != "${SLACK_APP_TOKEN}" || cfg.OAuth.BotToken != "${SLACK_BOT_TOKEN}" {
		t.Fatalf("oauth = %+v, want ${VAR} references", cfg.OAuth)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "xapp-1-test") || strings.Contains(string(raw), "xoxb-test") {
		t.Fatalf("raw token leaked into gateway.yaml:\n%s", raw)
	}
	// Tokens landed in .env.
	envData, err := os.ReadFile(r.EnvPath)
	if err != nil {
		t.Fatalf("read .env: %v", err)
	}
	if !strings.Contains(string(envData), "SLACK_APP_TOKEN=xapp-1-test") {
		t.Fatalf(".env missing token:\n%s", envData)
	}
	// access went to the store, NOT gateway.yaml.
	body, ok, err := s.GetSingleton(context.Background(), config.SingletonAccess)
	if err != nil || !ok {
		t.Fatalf("access singleton missing: ok=%v err=%v", ok, err)
	}
	var access config.AccessConfig
	_ = json.Unmarshal(body, &access)
	if access.AdminUser != "@admin" {
		t.Fatalf("stored admin_user = %q, want @admin", access.AdminUser)
	}
}

func TestInvoke_PreservesDatabaseBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	// Seed a gateway.yaml that already selects Postgres; setup.slack must keep it.
	seed := "oauth:\n  app_token: old\ndatabase:\n  backend: postgres\n  postgres:\n    dsn: ${MURTAUGH_DB_DSN}\n"
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	_, prov := newStore(t)
	tl := New(func() string { return path }, prov)
	if _, err := tl.Invoke(context.Background(), validArgs()); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	cfg := gateway(t, path)
	if cfg.Database.Backend != "postgres" || cfg.Database.Postgres.DSN != "${MURTAUGH_DB_DSN}" {
		t.Fatalf("database block not preserved: %+v", cfg.Database)
	}
}

func TestInvoke_DefaultAgentEnablesChat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.yaml")
	s, prov := newStore(t)
	tl := New(func() string { return path }, prov)

	args := validArgs()
	args["default_agent"] = "code"
	res, err := tl.Invoke(context.Background(), args)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.(Result).ChatEnabled {
		t.Fatal("chat should be enabled when a default agent is given")
	}
	body, ok, _ := s.GetSingleton(context.Background(), config.SingletonChat)
	if !ok {
		t.Fatal("chat singleton missing")
	}
	var chat config.ChatConfig
	_ = json.Unmarshal(body, &chat)
	if !chat.Enabled || chat.Defaults.Agent != "code" {
		t.Fatalf("stored chat = %+v, want enabled + agent code", chat)
	}
}
