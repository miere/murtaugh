package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/nodetoken"
)

type nodeTokensFactory func(t *testing.T) config.NodeTokenStore

func sqliteNodeTokensFactory(t *testing.T) nodeTokensFactory {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.db")
	return func(t *testing.T) config.NodeTokenStore {
		t.Helper()
		tokens, err := OpenNodeTokens(context.Background(),
			config.DatabaseConfig{Backend: config.BackendSQLite, SQLite: config.SQLiteConfig{Path: path}}, "", "")
		if err != nil {
			t.Fatalf("OpenNodeTokens(sqlite): %v", err)
		}
		t.Cleanup(func() { _ = tokens.Close() })
		return tokens
	}
}

func postgresNodeTokensFactory(t *testing.T) nodeTokensFactory {
	t.Helper()
	dsn := postgresTestDSN(t)
	return func(t *testing.T) config.NodeTokenStore {
		t.Helper()
		tokens, err := OpenNodeTokens(context.Background(),
			config.DatabaseConfig{Backend: config.BackendPostgres, Postgres: config.PostgresConfig{DSN: dsn}}, "", "")
		if err != nil {
			t.Fatalf("OpenNodeTokens(postgres): %v", err)
		}
		t.Cleanup(func() { _ = tokens.Close() })
		return tokens
	}
}

func firestoreNodeTokensFactory(t *testing.T) nodeTokensFactory {
	t.Helper()
	fsc := firestoreTestConfig(t)
	return func(t *testing.T) config.NodeTokenStore {
		t.Helper()
		tokens, err := OpenNodeTokens(context.Background(),
			config.DatabaseConfig{Backend: config.BackendFirestore, Firestore: fsc}, "", "")
		if err != nil {
			t.Fatalf("OpenNodeTokens(firestore): %v", err)
		}
		t.Cleanup(func() { _ = tokens.Close() })
		return tokens
	}
}

func uniqueNodeID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), nodeIDSeq.Add(1))
}

var nodeIDSeq atomic.Int64

func mintFor(t *testing.T, nodeID, userID, label string, createdAt, expiresAt time.Time) (config.NodeToken, string) {
	t.Helper()
	minted, err := nodetoken.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return config.NodeToken{
		Selector:   minted.Selector,
		SecretHash: string(minted.SecretHash),
		NodeID:     nodeID,
		UserID:     userID,
		Label:      label,
		CreatedAt:  createdAt,
		ExpiresAt:  expiresAt,
	}, minted.Token
}

func TestNodeTokenStore(t *testing.T) {
	created := time.Date(2026, 9, 8, 9, 0, 0, 0, time.UTC)

	for name, build := range map[string]func(*testing.T) nodeTokensFactory{
		"sqlite":    sqliteNodeTokensFactory,
		"postgres":  postgresNodeTokensFactory,
		"firestore": firestoreNodeTokensFactory,
	} {
		t.Run(name, func(t *testing.T) {
			t.Run("a stored credential round-trips", func(t *testing.T) {
				newStore := build(t)
				ctx := context.Background()
				node := uniqueNodeID("round-trip")

				expires := created.Add(24 * time.Hour)
				record, _ := mintFor(t, node, "U123", "mac mini", created, expires)
				if err := newStore(t).Put(ctx, record); err != nil {
					t.Fatalf("Put: %v", err)
				}

				got, found, err := newStore(t).BySelector(ctx, record.Selector)
				if err != nil || !found {
					t.Fatalf("BySelector: found=%v err=%v", found, err)
				}
				if got.NodeID != node || got.UserID != "U123" || got.Label != "mac mini" {
					t.Errorf("identity did not round-trip: %+v", got)
				}
				if got.SecretHash != record.SecretHash {
					t.Errorf("secret hash = %q, want %q", got.SecretHash, record.SecretHash)
				}
				if !got.CreatedAt.Equal(created) {
					t.Errorf("created-at = %v, want %v", got.CreatedAt, created)
				}
				if !got.ExpiresAt.Equal(expires) {
					t.Errorf("expires-at = %v, want %v", got.ExpiresAt, expires)
				}
				if !got.RevokedAt.IsZero() {
					t.Errorf("a fresh credential is revoked at %v", got.RevokedAt)
				}
			})

			t.Run("an absent selector is not an error", func(t *testing.T) {
				newStore := build(t)
				_, found, err := newStore(t).BySelector(context.Background(), "0123456789abcdef")
				if err != nil {
					t.Fatalf("BySelector on an unknown selector errored: %v", err)
				}
				if found {
					t.Fatal("BySelector invented a credential")
				}
			})

			t.Run("a zero expiry survives as the zero time", func(t *testing.T) {
				newStore := build(t)
				ctx := context.Background()
				record, _ := mintFor(t, uniqueNodeID("perpetual"), "U1", "", created, time.Time{})
				if err := newStore(t).Put(ctx, record); err != nil {
					t.Fatalf("Put: %v", err)
				}
				got, found, err := newStore(t).BySelector(ctx, record.Selector)
				if err != nil || !found {
					t.Fatalf("BySelector: found=%v err=%v", found, err)
				}
				if !got.ExpiresAt.IsZero() {
					t.Fatalf("expires-at = %v, want the zero time", got.ExpiresAt)
				}
				if !got.Live(created.Add(100 * 365 * 24 * time.Hour)) {
					t.Fatal("a credential with no expiry stopped being live")
				}
			})

			t.Run("an existing selector is refused, not overwritten", func(t *testing.T) {
				newStore := build(t)
				ctx := context.Background()
				store := newStore(t)

				first, _ := mintFor(t, uniqueNodeID("first"), "U1", "", created, time.Time{})
				if err := store.Put(ctx, first); err != nil {
					t.Fatalf("Put: %v", err)
				}
				second, _ := mintFor(t, uniqueNodeID("second"), "U2", "", created, time.Time{})
				second.Selector = first.Selector

				if err := store.Put(ctx, second); err == nil {
					t.Fatal("Put overwrote an existing credential; a live node would have been hijacked silently")
				}
				got, _, err := store.BySelector(ctx, first.Selector)
				if err != nil {
					t.Fatal(err)
				}
				if got.NodeID != first.NodeID {
					t.Fatalf("the stored credential now names %q, want %q", got.NodeID, first.NodeID)
				}
			})

			t.Run("an invalid record is refused", func(t *testing.T) {
				newStore := build(t)
				ctx := context.Background()
				store := newStore(t)

				valid, _ := mintFor(t, uniqueNodeID("invalid"), "U1", "", created, time.Time{})
				for name, mutate := range map[string]func(*config.NodeToken){
					"no node":       func(r *config.NodeToken) { r.NodeID = "" },
					"no user":       func(r *config.NodeToken) { r.UserID = "" },
					"no hash":       func(r *config.NodeToken) { r.SecretHash = "" },
					"no selector":   func(r *config.NodeToken) { r.Selector = "" },
					"no created-at": func(r *config.NodeToken) { r.CreatedAt = time.Time{} },
				} {
					record := valid
					mutate(&record)
					if err := store.Put(ctx, record); err == nil {
						t.Errorf("Put accepted a record with %s", name)
					}
				}
			})

			t.Run("listing is per node, newest first", func(t *testing.T) {
				newStore := build(t)
				ctx := context.Background()
				store := newStore(t)

				node, other := uniqueNodeID("listed"), uniqueNodeID("other")
				older, _ := mintFor(t, node, "U1", "older", created, time.Time{})
				newer, _ := mintFor(t, node, "U1", "newer", created.Add(time.Hour), time.Time{})
				foreign, _ := mintFor(t, other, "U2", "elsewhere", created, time.Time{})
				for _, r := range []config.NodeToken{newer, foreign, older} {
					if err := store.Put(ctx, r); err != nil {
						t.Fatalf("Put: %v", err)
					}
				}

				got, err := store.List(ctx, node)
				if err != nil {
					t.Fatalf("List: %v", err)
				}
				if len(got) != 2 {
					t.Fatalf("List(%q) returned %d credentials, want 2 (another node's must not appear)", node, len(got))
				}
				if got[0].Selector != newer.Selector || got[1].Selector != older.Selector {
					t.Errorf("List order = [%s %s], want newest first [%s %s]",
						got[0].Label, got[1].Label, newer.Label, older.Label)
				}

				all, err := store.List(ctx, "")
				if err != nil {
					t.Fatalf("List(all): %v", err)
				}
				if !containsSelector(all, foreign.Selector) {
					t.Error("List with no node id omitted another node's credential")
				}
			})

			t.Run("revoking marks the credential and keeps the first timestamp", func(t *testing.T) {
				newStore := build(t)
				ctx := context.Background()
				store := newStore(t)

				record, _ := mintFor(t, uniqueNodeID("revoked"), "U1", "", created, time.Time{})
				if err := store.Put(ctx, record); err != nil {
					t.Fatalf("Put: %v", err)
				}

				first := created.Add(time.Hour)
				got, found, err := store.Revoke(ctx, record.Selector, first)
				if err != nil || !found {
					t.Fatalf("Revoke: found=%v err=%v", found, err)
				}
				if !got.RevokedAt.Equal(first) {
					t.Fatalf("revoked-at = %v, want %v", got.RevokedAt, first)
				}
				if got.Live(first) {
					t.Fatal("a revoked credential still reports itself live")
				}

				again, found, err := store.Revoke(ctx, record.Selector, first.Add(time.Hour))
				if err != nil || !found {
					t.Fatalf("second Revoke: found=%v err=%v", found, err)
				}
				if !again.RevokedAt.Equal(first) {
					t.Errorf("a second revocation rewrote the timestamp to %v, want %v", again.RevokedAt, first)
				}
			})

			t.Run("revoking an unknown selector reports absence, not failure", func(t *testing.T) {
				newStore := build(t)
				_, found, err := newStore(t).Revoke(context.Background(), "ffffffffffffffff", created)
				if err != nil {
					t.Fatalf("Revoke on an unknown selector errored: %v", err)
				}
				if found {
					t.Fatal("Revoke claimed to revoke a credential that does not exist")
				}
			})
		})
	}
}

func containsSelector(records []config.NodeToken, selector string) bool {
	for _, r := range records {
		if r.Selector == selector {
			return true
		}
	}
	return false
}

// Reads every stored value because the interface only limits what a caller can ask for;
// plaintext in an unused column would pass every other test.
func TestNodeTokenPlaintextIsNotStored(t *testing.T) {
	created := time.Date(2026, 9, 8, 9, 0, 0, 0, time.UTC)

	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.db")
		ctx := context.Background()
		store, err := OpenNodeTokens(ctx,
			config.DatabaseConfig{Backend: config.BackendSQLite, SQLite: config.SQLiteConfig{Path: path}}, "", "")
		if err != nil {
			t.Fatalf("OpenNodeTokens: %v", err)
		}
		defer func() { _ = store.Close() }()

		record, plaintext := mintFor(t, uniqueNodeID("leak"), "U1", "somewhere", created, time.Time{})
		if err := store.Put(ctx, record); err != nil {
			t.Fatalf("Put: %v", err)
		}

		db, err := openSQLiteDB(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer func() { _ = db.Close() }()
		assertNoPlaintext(t, dumpAllCells(t, db, "node_tokens"), plaintext)
	})

	t.Run("firestore", func(t *testing.T) {
		fsc := firestoreTestConfig(t)
		ctx := context.Background()
		store, err := OpenNodeTokens(ctx, config.DatabaseConfig{Backend: config.BackendFirestore, Firestore: fsc}, "", "")
		if err != nil {
			t.Fatalf("OpenNodeTokens: %v", err)
		}
		defer func() { _ = store.Close() }()

		record, plaintext := mintFor(t, uniqueNodeID("leak"), "U1", "somewhere", created, time.Time{})
		if err := store.Put(ctx, record); err != nil {
			t.Fatalf("Put: %v", err)
		}

		client, err := newFirestoreClient(ctx, fsc)
		if err != nil {
			t.Fatalf("firestore client: %v", err)
		}
		defer func() { _ = client.Close() }()
		assertNoPlaintext(t, dumpAllFields(t, ctx, client.Collection(fsc.EffectiveCollection()+"_node_tokens")), plaintext)
	})
}

func assertNoPlaintext(t *testing.T, values []string, plaintext string) {
	t.Helper()
	if len(values) == 0 {
		t.Fatal("no stored values were read back; the check would pass vacuously")
	}
	credential, err := nodetoken.Parse(plaintext)
	if err != nil {
		t.Fatalf("Parse the minted token: %v", err)
	}
	for _, value := range values {
		switch {
		case strings.Contains(value, plaintext):
			t.Errorf("a stored value contains the whole token")
		case strings.Contains(value, credential.Secret):
			t.Errorf("a stored value contains the token's secret half")
		case strings.Contains(value, nodetoken.Prefix):
			t.Errorf("a stored value contains %q, so something token-shaped was persisted: %q", nodetoken.Prefix, value)
		}
	}
}

func dumpAllCells(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query("SELECT * FROM " + table) //nolint:gosec // table is a constant
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer func() { _ = rows.Close() }()

	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	var out []string
	for rows.Next() {
		cells := make([]any, len(columns))
		for i := range cells {
			cells[i] = new(sql.NullString)
		}
		if err := rows.Scan(cells...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		for _, cell := range cells {
			if v := cell.(*sql.NullString); v.Valid {
				out = append(out, v.String)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func dumpAllFields(t *testing.T, ctx context.Context, coll *firestore.CollectionRef) []string {
	t.Helper()
	iter := coll.Documents(ctx)
	defer iter.Stop()

	var out []string
	for {
		snap, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatalf("iterate: %v", err)
		}
		out = append(out, snap.Ref.ID)
		for _, value := range snap.Data() {
			out = append(out, fmt.Sprint(value))
		}
	}
	return out
}

func TestNodeTokenMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.db")
	ctx := context.Background()
	dbc := config.DatabaseConfig{Backend: config.BackendSQLite, SQLite: config.SQLiteConfig{Path: path}}

	first, err := OpenNodeTokens(ctx, dbc, "", "")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	record, _ := mintFor(t, uniqueNodeID("migrate"), "U1", "", time.Now().UTC().Truncate(time.Millisecond), time.Time{})
	if err := first.Put(ctx, record); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := OpenNodeTokens(ctx, dbc, "", "")
	if err != nil {
		t.Fatalf("second open (migrations are not idempotent): %v", err)
	}
	defer func() { _ = second.Close() }()
	if _, found, err := second.BySelector(ctx, record.Selector); err != nil || !found {
		t.Fatalf("the credential did not survive a re-open: found=%v err=%v", found, err)
	}
}

// As a config section, credentials would be loaded by every process, printed by
// `cfg show` and copied by `cfg db migrate`.
func TestNodeTokensAreNotConfig(t *testing.T) {
	for _, section := range config.AllSections {
		if strings.Contains(section, "node") && strings.Contains(section, "token") {
			t.Errorf("node tokens are a config section (%q); they must stay out of the loaded Config", section)
		}
	}
	for _, key := range config.AllSingletons {
		if strings.Contains(key, "node") && strings.Contains(key, "token") {
			t.Errorf("node tokens are a config singleton (%q); they must stay out of the loaded Config", key)
		}
	}
}
