package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTool_Metadata(t *testing.T) {
	tl := New(func() string { return "" })
	if tl.Name() != "setup.bootstrap" {
		t.Fatalf("Name() = %q, want setup.bootstrap", tl.Name())
	}
	if strings.TrimSpace(tl.Description()) == "" {
		t.Fatal("Description() must not be blank")
	}
	schema := tl.InputSchema()
	if schema == nil || schema.Properties["force"] == nil {
		t.Fatalf("InputSchema() must declare the optional force flag, got %v", schema)
	}
}

func TestInvoke_FreshDirSeedsAndReportsCreated(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "slack.yaml")
	tl := New(func() string { return configPath })

	res, err := tl.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if len(r.Created) == 0 {
		t.Fatalf("Created should be non-empty on fresh dir, got %+v", r)
	}
	if len(r.Preserved) != 0 {
		t.Fatalf("Preserved should be empty on fresh dir, got %+v", r.Preserved)
	}
	for _, p := range r.Created {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("reported created file missing: %s (%v)", p, err)
		}
	}
	// slack.yaml at configPath is mandatory; all bootstrap reports it.
	if !containsPath(r.Created, configPath) {
		t.Fatalf("Created should mention %s, got %+v", configPath, r.Created)
	}
}

func TestInvoke_SecondRunPreservesEverything(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "slack.yaml")
	tl := New(func() string { return configPath })

	if _, err := tl.Invoke(context.Background(), nil); err != nil {
		t.Fatalf("first Invoke: %v", err)
	}
	res, err := tl.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("second Invoke: %v", err)
	}
	r := res.(Result)
	if len(r.Created) != 0 {
		t.Fatalf("Created should be empty on second run, got %+v", r.Created)
	}
	if len(r.Preserved) == 0 {
		t.Fatalf("Preserved should be non-empty on second run, got %+v", r)
	}
	if !containsPath(r.Preserved, configPath) {
		t.Fatalf("Preserved should mention %s, got %+v", configPath, r.Preserved)
	}
}

func TestInvoke_MixedReportsBothBuckets(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "slack.yaml")
	// Pre-seed slack.yaml only; agents/jobs should be created.
	if err := os.WriteFile(configPath, []byte("existing: true\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tl := New(func() string { return configPath })

	res, err := tl.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if !containsPath(r.Preserved, configPath) {
		t.Fatalf("Preserved should mention pre-existing %s, got %+v", configPath, r.Preserved)
	}
	if len(r.Created) == 0 {
		t.Fatalf("Created should mention newly-seeded files, got %+v", r.Created)
	}
}

func TestResult_String_Summarises(t *testing.T) {
	r := Result{Created: []string{"/tmp/a"}, Preserved: []string{"/tmp/b"}}
	got := r.String()
	if !strings.Contains(got, "1 created") || !strings.Contains(got, "1 preserved") {
		t.Fatalf("String() = %q, want it to mention counts", got)
	}
}

func containsPath(haystack []string, want string) bool {
	for _, p := range haystack {
		if p == want {
			return true
		}
	}
	return false
}

// A runtime node's root is defined by what it does NOT contain: no `oauth:`
// block, and an .env template naming no SLACK_* variable. Seeding it from the
// gateway skeleton would put a file advertising ${SLACK_APP_TOKEN} on every
// laptop in the fleet — an invitation to fill in credentials #170 says that
// machine must never hold.
func TestRoleRuntimeSeedsARootWithNoSlackCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	tl := New(func() string { return path })

	if _, err := tl.Invoke(context.Background(), map[string]any{"role": "runtime"}); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	body := readFile(t, path)
	if strings.Contains(body, "${SLACK_APP_TOKEN}") {
		t.Fatalf("a node's config.yaml references the workspace's Slack token:\n%s", body)
	}
	if env := readFile(t, filepath.Join(dir, ".env")); strings.Contains(env, "SLACK_APP_TOKEN=") {
		t.Fatalf("a node's .env has a slot for the workspace's Slack token:\n%s", env)
	}

	// And the default is unchanged, which is what every existing caller gets.
	gwDir := t.TempDir()
	gwPath := filepath.Join(gwDir, "config.yaml")
	if _, err := New(func() string { return gwPath }).Invoke(context.Background(), map[string]any{}); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(readFile(t, gwPath), "${SLACK_APP_TOKEN}") {
		t.Fatal("the default role no longer seeds the gateway skeleton")
	}
}

// An unknown role is refused rather than defaulted. The two roots differ by a
// credential block, and quietly seeding the wrong one over a typo puts Slack
// tokens where they must never be.
func TestAnUnknownRoleIsRefused(t *testing.T) {
	dir := t.TempDir()
	tl := New(func() string { return filepath.Join(dir, "config.yaml") })
	_, err := tl.Invoke(context.Background(), map[string]any{"role": "node"})
	if err == nil {
		t.Fatal("an unknown role was accepted")
	}
	if !strings.Contains(err.Error(), "node") {
		t.Fatalf("the refusal did not name the value: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}
