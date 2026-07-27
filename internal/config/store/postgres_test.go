package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

// openPostgresTestStore opens the store against MURTAUGH_TEST_POSTGRES_DSN,
// skipping the whole test when it is unset so `go test ./...` stays green
// without Docker. It truncates the config tables first so each test starts
// clean against a shared database.
func openPostgresTestStore(t *testing.T) config.Store {
	t.Helper()
	dsn := os.Getenv("MURTAUGH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set MURTAUGH_TEST_POSTGRES_DSN (e.g. via docker compose up -d) to run Postgres tests")
	}
	ctx := context.Background()
	s, err := Open(ctx, config.DatabaseConfig{Backend: config.BackendPostgres, Postgres: config.PostgresConfig{DSN: dsn}})
	if err != nil {
		t.Fatalf("open postgres store: %v", err)
	}
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(ctx, "TRUNCATE config_items, config_singletons"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestPostgresRoundTrip runs the core store behaviours against a real Postgres,
// exercising the $n placeholders, ON CONFLICT upsert, and JSONB body column.
func TestPostgresRoundTrip(t *testing.T) {
	s := openPostgresTestStore(t)
	ctx := context.Background()

	agent := config.AgentProfile{Native: &config.NativeProfile{Provider: "gemini", Model: "gemini-2.5-pro", APIKeyEnv: "K"}}
	if err := s.UpsertItem(ctx, config.SectionAgent, "code", agent); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	body, ok, err := s.GetItem(ctx, config.SectionAgent, "code")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	var got config.AgentProfile
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Native == nil || got.Native.Model != "gemini-2.5-pro" {
		t.Fatalf("round-trip lost body: %s", body)
	}

	if err := s.PutSingleton(ctx, config.SingletonChat, config.ChatConfig{Enabled: true, Defaults: config.ChatDefaults{Agent: "code"}}); err != nil {
		t.Fatalf("put singleton: %v", err)
	}
	cfg, err := s.Load(ctx, config.Config{OAuth: config.OAuthConfig{AppToken: "x", BotToken: "x"}})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := cfg.Agents["code"]; !ok || !cfg.Chat.Enabled {
		t.Fatalf("assembled config wrong: %+v", cfg)
	}

	deleted, err := s.DeleteItem(ctx, config.SectionAgent, "code")
	if err != nil || !deleted {
		t.Fatalf("delete: deleted=%v err=%v", deleted, err)
	}
}

// TestSQLiteToPostgresMigration proves a SQLite snapshot restores into Postgres
// and assembles an identical Config — the backbone of `cfg db migrate`.
func TestSQLiteToPostgresMigration(t *testing.T) {
	pg := openPostgresTestStore(t)
	ctx := context.Background()

	// Build a SQLite store with representative content.
	sqliteStore, err := Open(ctx, config.DatabaseConfig{
		Backend: config.BackendSQLite,
		SQLite:  config.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")},
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer sqliteStore.Close()
	if err := sqliteStore.UpsertItem(ctx, config.SectionAgent, "code", config.AgentProfile{ClaudeCode: &config.ClaudeCodeProfile{Command: "claude"}}); err != nil {
		t.Fatal(err)
	}
	if err := sqliteStore.UpsertItem(ctx, config.SectionJob, "nightly", config.JobProfile{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	if err := sqliteStore.PutSingleton(ctx, config.SingletonAccess, config.AccessConfig{AdminUser: "U1"}); err != nil {
		t.Fatal(err)
	}

	snap, err := sqliteStore.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := pg.Restore(ctx, snap); err != nil {
		t.Fatalf("restore into postgres: %v", err)
	}

	base := config.Config{OAuth: config.OAuthConfig{AppToken: "x", BotToken: "x"}}
	sqliteCfg, err := sqliteStore.Load(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	pgCfg, err := pg.Load(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sqliteCfg, pgCfg) {
		t.Fatalf("cross-backend config mismatch:\n sqlite=%+v\n pg=%+v", sqliteCfg, pgCfg)
	}
}
