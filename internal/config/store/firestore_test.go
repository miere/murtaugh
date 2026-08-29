package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

// firestoreTestConfig returns the emulator-backed settings, skipping the whole
// test when FIRESTORE_EMULATOR_HOST is unset so `go test ./...` stays green
// without Docker — the same bargain the Postgres tests strike.
//
// The project ID is explicit rather than detected: the emulator accepts any
// project and performs no auth, so ADC detection would fail for want of
// credentials that are not needed.
func firestoreTestConfig(t *testing.T) config.FirestoreConfig {
	t.Helper()
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("set FIRESTORE_EMULATOR_HOST (e.g. via docker compose up -d) to run Firestore tests")
	}
	// The root collection is unique per test AND per run. Per-test alone is not
	// enough: the emulator keeps its data for as long as it is up and offers no
	// truncate, so a second `go test` would find the previous run's documents.
	// That is harmless for the store tests, which only add rows, but fatal for
	// the lock tests, which contend on a single document — a leftover live lease
	// makes every acquisition in the new run fail.
	return config.FirestoreConfig{
		ProjectID:  "murtaugh-test",
		Collection: fmt.Sprintf("test_%s_%d", strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_")), testRunID),
	}
}

// testRunID distinguishes one `go test` invocation from the next against a
// long-lived emulator. It is read once at process start.
var testRunID = time.Now().UnixNano()

// openFirestoreTestStore opens a store against the emulator.
func openFirestoreTestStore(t *testing.T) config.Store {
	t.Helper()
	s, err := Open(context.Background(), config.DatabaseConfig{
		Backend:   config.BackendFirestore,
		Firestore: firestoreTestConfig(t),
	}, "", "")
	if err != nil {
		t.Fatalf("open firestore store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestFirestoreRoundTrip runs the core store behaviours against a real
// Firestore, exercising the document mapping, the section query, and the
// server-timestamped writes.
func TestFirestoreRoundTrip(t *testing.T) {
	s := openFirestoreTestStore(t)
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
	// A second delete must report absence rather than inventing a row: Firestore
	// deletes are no-ops on missing documents, so this is the behaviour the
	// implementation has to add on top.
	deleted, err = s.DeleteItem(ctx, config.SectionAgent, "code")
	if err != nil || deleted {
		t.Fatalf("second delete: deleted=%v err=%v; want false", deleted, err)
	}
}

// TestFirestoreMissingReadsAreNotErrors pins that an absent item or singleton is
// an ordinary "not found" rather than a failure, which is how the SQL backends
// translate sql.ErrNoRows and what the config loader depends on for an
// unconfigured section.
func TestFirestoreMissingReadsAreNotErrors(t *testing.T) {
	s := openFirestoreTestStore(t)
	ctx := context.Background()

	if _, ok, err := s.GetItem(ctx, config.SectionAgent, "absent"); err != nil || ok {
		t.Errorf("GetItem(absent): ok=%v err=%v; want false, nil", ok, err)
	}
	if _, ok, err := s.GetSingleton(ctx, config.SingletonAccess); err != nil || ok {
		t.Errorf("GetSingleton(absent): ok=%v err=%v; want false, nil", ok, err)
	}
	rows, err := s.ListItems(ctx, config.SectionJob)
	if err != nil {
		t.Fatalf("ListItems on an empty section: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("ListItems on an empty section returned %d rows", len(rows))
	}
}

// TestFirestoreSectionsDoNotLeak checks the section query actually filters:
// entities of different sections share one collection, so a broken filter would
// assemble jobs into the agent map and fail validation in a confusing place.
func TestFirestoreSectionsDoNotLeak(t *testing.T) {
	s := openFirestoreTestStore(t)
	ctx := context.Background()

	if err := s.UpsertItem(ctx, config.SectionAgent, "shared", config.AgentProfile{ClaudeCode: &config.ClaudeCodeProfile{Command: "claude"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertItem(ctx, config.SectionJob, "shared", config.JobProfile{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	agents, err := s.ListItems(ctx, config.SectionAgent)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 {
		t.Fatalf("agent section returned %d rows, want 1: %v", len(agents), agents)
	}
	// Same name, different section: deleting one must leave the other standing.
	if _, err := s.DeleteItem(ctx, config.SectionAgent, "shared"); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.ListItems(ctx, config.SectionJob)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Errorf("deleting agent/shared also removed job/shared")
	}
}

// TestSQLiteToFirestoreMigration proves a SQLite snapshot restores into
// Firestore and assembles an identical Config. This is the backbone of
// `cfg db migrate`, and it is the check that matters most for the fallback
// story: moving to Firestore for leader election must not change the config a
// node loads.
func TestSQLiteToFirestoreMigration(t *testing.T) {
	fs := openFirestoreTestStore(t)
	ctx := context.Background()

	sqliteStore, err := Open(ctx, config.DatabaseConfig{
		Backend: config.BackendSQLite,
		SQLite:  config.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")},
	}, "", "")
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
	if err := fs.Restore(ctx, snap); err != nil {
		t.Fatalf("restore into firestore: %v", err)
	}

	base := config.Config{OAuth: config.OAuthConfig{AppToken: "x", BotToken: "x"}}
	sqliteCfg, err := sqliteStore.Load(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	fsCfg, err := fs.Load(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sqliteCfg, fsCfg) {
		t.Fatalf("cross-backend config mismatch:\n sqlite=%+v\n firestore=%+v", sqliteCfg, fsCfg)
	}
}

// TestItemDocIDIsSafeAndUnambiguous covers the document-ID mapping without
// needing an emulator. Firestore rejects "/" in an ID and reserves the __.*__
// pattern, and config entity names are free-form, so the encoding has to
// survive both without two distinct entities colliding on one document.
func TestItemDocIDIsSafeAndUnambiguous(t *testing.T) {
	names := []string{
		"code",
		"with/slash",
		"with space",
		"__reserved__",
		".",
		"..",
		"unicode-ç-ok",
		"with~tilde",
	}
	seen := make(map[string]string, len(names))
	for _, name := range names {
		id := itemDocID(config.SectionAgent, name)
		if strings.Contains(id, "/") {
			t.Errorf("itemDocID(%q) = %q contains a slash, which Firestore rejects", name, id)
		}
		if strings.HasPrefix(id, "__") && strings.HasSuffix(id, "__") {
			t.Errorf("itemDocID(%q) = %q matches Firestore's reserved __.*__ pattern", name, id)
		}
		if id == "." || id == ".." {
			t.Errorf("itemDocID(%q) = %q, which Firestore rejects", name, id)
		}
		if prior, dup := seen[id]; dup {
			t.Errorf("itemDocID collision: %q and %q both map to %q", prior, name, id)
		}
		seen[id] = name
	}

	// Distinct sections must not collide on an identically-named entity.
	if itemDocID(config.SectionAgent, "shared") == itemDocID(config.SectionJob, "shared") {
		t.Error("sections collide on document ID; one entity would overwrite the other")
	}

	// The escaping must be reversible, which is what lets an operator find a
	// document in the console from an entity name.
	decoded, err := url.PathUnescape(strings.TrimPrefix(itemDocID(config.SectionAgent, "with/slash"), config.SectionAgent+"~"))
	if err != nil || decoded != "with/slash" {
		t.Errorf("document ID does not decode back to the name: got %q err=%v", decoded, err)
	}
}

// TestFirestoreConfigDefaults pins the zero-value behaviour an operator relies
// on: nothing but `backend: firestore` should work on a GCP host, because ADC
// already knows the project and Firestore already has a default database.
func TestFirestoreConfigDefaults(t *testing.T) {
	var fsc config.FirestoreConfig
	if got := fsc.EffectiveCollection(); got != config.DefaultFirestoreCollection {
		t.Errorf("EffectiveCollection() = %q, want %q", got, config.DefaultFirestoreCollection)
	}
	if got := fsc.EffectiveDatabaseID(); got != config.DefaultFirestoreDatabase {
		t.Errorf("EffectiveDatabaseID() = %q, want %q", got, config.DefaultFirestoreDatabase)
	}
	if got := fsc.EffectiveCredentialsFile(); got != "" {
		t.Errorf("EffectiveCredentialsFile() = %q, want empty (meaning ADC)", got)
	}

	set := config.FirestoreConfig{Collection: " custom ", DatabaseID: " named ", CredentialsFile: " /tmp/key.json "}
	if got := set.EffectiveCollection(); got != "custom" {
		t.Errorf("EffectiveCollection() = %q, want %q", got, "custom")
	}
	if got := set.EffectiveDatabaseID(); got != "named" {
		t.Errorf("EffectiveDatabaseID() = %q, want %q", got, "named")
	}
	if got := set.EffectiveCredentialsFile(); got != "/tmp/key.json" {
		t.Errorf("EffectiveCredentialsFile() = %q, want %q", got, "/tmp/key.json")
	}
}

// TestDatabaseConfigIsZeroSeesFirestore guards the YAML→DB auto-migration
// trigger: IsZero is how startup decides a bootstrap file predates the store
// feature. A Firestore-only `database:` block that read as zero would send a
// configured node through the legacy migration path on every boot.
func TestDatabaseConfigIsZeroSeesFirestore(t *testing.T) {
	if (config.DatabaseConfig{}).IsZero() != true {
		t.Error("an empty database block should read as zero")
	}
	configured := config.DatabaseConfig{
		Backend:   config.BackendFirestore,
		Firestore: config.FirestoreConfig{ProjectID: "p"},
	}
	if configured.IsZero() {
		t.Error("a Firestore-configured database block read as zero; startup would re-run the legacy YAML migration")
	}
	// Even without `backend:`, firestore settings alone mean "configured".
	onlyFirestore := config.DatabaseConfig{Firestore: config.FirestoreConfig{ProjectID: "p"}}
	if onlyFirestore.IsZero() {
		t.Error("firestore settings alone read as zero")
	}
}
