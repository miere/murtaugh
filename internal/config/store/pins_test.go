package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

type pinsFactory func(t *testing.T) config.ConversationPinStore

func sqlitePinsFactory(t *testing.T) pinsFactory {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.db")
	return func(t *testing.T) config.ConversationPinStore {
		t.Helper()
		pins, err := OpenConversationPins(context.Background(),
			config.DatabaseConfig{Backend: config.BackendSQLite, SQLite: config.SQLiteConfig{Path: path}}, "", "")
		if err != nil {
			t.Fatalf("OpenConversationPins(sqlite): %v", err)
		}
		t.Cleanup(func() { _ = pins.Close() })
		return pins
	}
}

func postgresPinsFactory(t *testing.T) pinsFactory {
	t.Helper()
	dsn := postgresTestDSN(t)
	return func(t *testing.T) config.ConversationPinStore {
		t.Helper()
		pins, err := OpenConversationPins(context.Background(),
			config.DatabaseConfig{Backend: config.BackendPostgres, Postgres: config.PostgresConfig{DSN: dsn}}, "", "")
		if err != nil {
			t.Fatalf("OpenConversationPins(postgres): %v", err)
		}
		t.Cleanup(func() { _ = pins.Close() })
		return pins
	}
}

func firestorePinsFactory(t *testing.T) pinsFactory {
	t.Helper()
	fsc := firestoreTestConfig(t)
	return func(t *testing.T) config.ConversationPinStore {
		t.Helper()
		pins, err := OpenConversationPins(context.Background(),
			config.DatabaseConfig{Backend: config.BackendFirestore, Firestore: fsc}, "", "")
		if err != nil {
			t.Fatalf("OpenConversationPins(firestore): %v", err)
		}
		t.Cleanup(func() { _ = pins.Close() })
		return pins
	}
}

func TestConversationPinStore(t *testing.T) {
	elected := time.Date(2026, 9, 8, 9, 0, 0, 0, time.UTC)

	for name, build := range map[string]func(*testing.T) pinsFactory{
		"sqlite":    sqlitePinsFactory,
		"postgres":  postgresPinsFactory,
		"firestore": firestorePinsFactory,
	} {
		t.Run(name, func(t *testing.T) {
			t.Run("a pin survives the gateway that wrote it", func(t *testing.T) {
				newStore := build(t)
				ctx := context.Background()
				ref := config.ConversationRef{TeamID: "T1", ChannelID: uniqueNodeID("C"), ThreadTS: "1.1"}

				if err := newStore(t).Put(ctx, config.ConversationPin{
					Conversation: ref, NodeID: "node-a", UserID: "U1", ElectedAt: elected,
				}); err != nil {
					t.Fatalf("Put: %v", err)
				}

				got, found, err := newStore(t).Get(ctx, ref)
				if err != nil || !found {
					t.Fatalf("Get after failover: found=%v err=%v", found, err)
				}
				if got.NodeID != "node-a" || got.UserID != "U1" {
					t.Fatalf("the pin changed across gateways: %+v", got)
				}
				if !got.ElectedAt.Equal(elected) {
					t.Fatalf("elected-at did not survive: got %v want %v", got.ElectedAt, elected)
				}
			})

			t.Run("a re-election overwrites rather than adds", func(t *testing.T) {
				newStore := build(t)
				ctx := context.Background()
				store := newStore(t)
				ref := config.ConversationRef{TeamID: "T1", ChannelID: uniqueNodeID("C"), ThreadTS: "1.1"}

				if err := store.Put(ctx, config.ConversationPin{
					Conversation: ref, NodeID: "node-a", UserID: "U1", ElectedAt: elected,
				}); err != nil {
					t.Fatalf("Put: %v", err)
				}
				later := elected.Add(time.Hour)
				if err := store.Put(ctx, config.ConversationPin{
					Conversation: ref, NodeID: "node-b", UserID: "U2", ElectedAt: later,
				}); err != nil {
					t.Fatalf("Put again: %v", err)
				}

				got, found, err := newStore(t).Get(ctx, ref)
				if err != nil || !found {
					t.Fatalf("Get: found=%v err=%v", found, err)
				}
				if got.NodeID != "node-b" || got.UserID != "U2" || !got.ElectedAt.Equal(later) {
					t.Fatalf("the re-election did not replace the pin: %+v", got)
				}
			})

			t.Run("the four key fields are all part of the key", func(t *testing.T) {
				newStore := build(t)
				ctx := context.Background()
				store := newStore(t)
				channel := uniqueNodeID("C")

				base := config.ConversationRef{TeamID: "T1", ChannelID: channel, ThreadTS: "1.1"}
				dm := base
				dm.DM = true
				thread := base
				thread.ThreadTS = "2.2"

				for ref, node := range map[config.ConversationRef]string{
					base: "node-a", dm: "node-b", thread: "node-c",
				} {
					if err := store.Put(ctx, config.ConversationPin{
						Conversation: ref, NodeID: node, UserID: "U1", ElectedAt: elected,
					}); err != nil {
						t.Fatalf("Put %+v: %v", ref, err)
					}
				}
				for ref, want := range map[config.ConversationRef]string{
					base: "node-a", dm: "node-b", thread: "node-c",
				} {
					got, found, err := newStore(t).Get(ctx, ref)
					if err != nil || !found {
						t.Fatalf("Get %+v: found=%v err=%v", ref, found, err)
					}
					if got.NodeID != want {
						t.Fatalf("%+v resolved to %q, want %q", ref, got.NodeID, want)
					}
				}
			})

			t.Run("an undelegated conversation is an ordinary answer", func(t *testing.T) {
				newStore := build(t)
				ctx := context.Background()
				ref := config.ConversationRef{TeamID: "T1", ChannelID: uniqueNodeID("C")}

				_, found, err := newStore(t).Get(ctx, ref)
				if err != nil {
					t.Fatalf("Get: %v", err)
				}
				if found {
					t.Fatal("a conversation nobody delegated came back pinned")
				}
			})

			t.Run("an unpinnable record is refused", func(t *testing.T) {
				ctx := context.Background()
				store := build(t)(t)
				if err := store.Put(ctx, config.ConversationPin{
					NodeID: "node-a", ElectedAt: elected,
				}); err == nil {
					t.Fatal("a pin with no conversation was accepted")
				}
				if err := store.Put(ctx, config.ConversationPin{
					Conversation: config.ConversationRef{ChannelID: "C1"}, ElectedAt: elected,
				}); err == nil {
					t.Fatal("a pin naming no node was accepted")
				}
			})
		})
	}
}
