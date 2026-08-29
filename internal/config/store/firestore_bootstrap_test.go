package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

// writeTestBootstrap lays down a minimal bootstrap file for rewrite tests.
func writeTestBootstrap(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}
	return path
}

// TestRewriteDatabaseBlockToFirestore covers the last step of
// `cfg db migrate --to firestore`: the bootstrap must end up pointing at the
// new store, and — the part worth testing — must still reference the Slack
// tokens as ${VAR} rather than baking in the expanded secrets.
func TestRewriteDatabaseBlockToFirestore(t *testing.T) {
	t.Setenv("SLACK_APP_TOKEN", "xapp-secret-value")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-secret-value")
	path := writeTestBootstrap(t, `
oauth:
  app_token: ${SLACK_APP_TOKEN}
  bot_token: ${SLACK_BOT_TOKEN}
database:
  backend: sqlite
  sqlite:
    path: /tmp/old.db
`)

	err := RewriteDatabaseBlock(path, config.DatabaseConfig{
		Backend: config.BackendFirestore,
		Firestore: config.FirestoreConfig{
			ProjectID:  "my-project",
			Collection: "murtaugh",
		},
	})
	if err != nil {
		t.Fatalf("RewriteDatabaseBlock: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	out := string(raw)

	if strings.Contains(out, "xapp-secret-value") || strings.Contains(out, "xoxb-secret-value") {
		t.Fatalf("rewrite baked expanded secrets into config.yaml:\n%s", out)
	}
	if !strings.Contains(out, "${SLACK_APP_TOKEN}") {
		t.Errorf("rewrite lost the ${VAR} token reference:\n%s", out)
	}
	if strings.Contains(out, "sqlite") {
		t.Errorf("rewrite left the previous backend's block behind:\n%s", out)
	}

	// Re-parse through the real loader: the rewritten file must open the new
	// store, not merely look right.
	boot, err := config.LoadBootstrap(path)
	if err != nil {
		t.Fatalf("LoadBootstrap on the rewritten file: %v", err)
	}
	if got := boot.Database.EffectiveBackend(); got != config.BackendFirestore {
		t.Errorf("backend = %q, want %q", got, config.BackendFirestore)
	}
	if got := boot.Database.Firestore.ProjectID; got != "my-project" {
		t.Errorf("project_id = %q, want %q", got, "my-project")
	}
	if boot.Database.IsZero() {
		t.Error("rewritten database block reads as zero; startup would re-run the legacy YAML migration")
	}
}

// TestRewriteDatabaseBlockFirestoreDefaultsStayImplicit checks a fully-defaulted
// Firestore target writes just `backend: firestore`. Freezing the resolved
// defaults into YAML would pin a project that ADC was supposed to supply,
// silently breaking the config when the same file is deployed elsewhere.
func TestRewriteDatabaseBlockFirestoreDefaultsStayImplicit(t *testing.T) {
	t.Setenv("SLACK_APP_TOKEN", "xapp-x")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-x")
	path := writeTestBootstrap(t, "oauth:\n  app_token: ${SLACK_APP_TOKEN}\n  bot_token: ${SLACK_BOT_TOKEN}\ndatabase:\n  backend: sqlite\n")

	if err := RewriteDatabaseBlock(path, config.DatabaseConfig{Backend: config.BackendFirestore}); err != nil {
		t.Fatalf("RewriteDatabaseBlock: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	out := string(raw)
	if !strings.Contains(out, "backend: firestore") {
		t.Fatalf("backend not written:\n%s", out)
	}
	for _, frozen := range []string{"(default)", "project_id", "collection", "credentials_file"} {
		if strings.Contains(out, frozen) {
			t.Errorf("rewrite froze the default %q into YAML:\n%s", frozen, out)
		}
	}
}
