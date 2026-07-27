package define

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/store"
)

// newStore opens a fresh SQLite config store in a temp dir and returns it with a
// provider closure suitable for New.
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

func getJob(t *testing.T, s config.Store, name string) (config.JobProfile, bool) {
	t.Helper()
	body, ok, err := s.GetItem(context.Background(), config.SectionJob, name)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if !ok {
		return config.JobProfile{}, false
	}
	var job config.JobProfile
	if err := json.Unmarshal(body, &job); err != nil {
		t.Fatalf("unmarshal job: %v", err)
	}
	return job, true
}

func nilStore() StoreProvider { return func() (config.Store, error) { return nil, nil } }

func TestTool_Metadata(t *testing.T) {
	tl := New(nilStore())
	if tl.Name() != "jobs.define" {
		t.Fatalf("Name() = %q, want jobs.define", tl.Name())
	}
	schema := tl.InputSchema()
	required := map[string]bool{}
	for _, r := range schema.Required {
		required[r] = true
	}
	for _, want := range []string{"name", "command"} {
		if !required[want] {
			t.Fatalf("required missing %q (have %v)", want, schema.Required)
		}
	}
}

func TestInvoke_CreatesNewJob(t *testing.T) {
	s, prov := newStore(t)
	tl := New(prov)

	res, err := tl.Invoke(context.Background(), map[string]any{
		"name":    "hello",
		"command": "/bin/echo",
		"args":    []any{"hi"},
		"timeout": "30s",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if !r.Created {
		t.Fatal("Result.Created = false, want true on first write")
	}
	job, ok := getJob(t, s, "hello")
	if !ok {
		t.Fatal("job not stored")
	}
	if job.Command != "/bin/echo" {
		t.Fatalf("job.Command = %q, want /bin/echo", job.Command)
	}
	if len(job.Args) != 1 || job.Args[0] != "hi" {
		t.Fatalf("job.Args = %v, want [hi]", job.Args)
	}
}

func TestRequiresApproval_AlwaysTrue(t *testing.T) {
	tl := New(nilStore())
	if !tl.RequiresApproval(nil) {
		t.Fatal("RequiresApproval(nil) = false, want true")
	}
	if !tl.RequiresApproval(map[string]any{"name": "x", "command": "/bin/echo"}) {
		t.Fatal("RequiresApproval = false, want true for any args")
	}
}

func TestApprovalSummary(t *testing.T) {
	tl := New(nilStore())

	t.Run("cron", func(t *testing.T) {
		got := tl.ApprovalSummary(map[string]any{
			"name":     "nightly",
			"command":  "/bin/backup",
			"args":     []any{"--all"},
			"schedule": "0 2 * * *",
		})
		for _, want := range []string{"nightly", "/bin/backup", "--all", "cron 0 2 * * *"} {
			if !strings.Contains(got, want) {
				t.Fatalf("ApprovalSummary = %q, want it to contain %q", got, want)
			}
		}
	})

	t.Run("every", func(t *testing.T) {
		got := tl.ApprovalSummary(map[string]any{
			"name":    "tick",
			"command": "/bin/echo",
			"every":   "1h",
		})
		if !strings.Contains(got, "every 1h") || !strings.Contains(got, "/bin/echo") {
			t.Fatalf("ApprovalSummary = %q, want every 1h + command", got)
		}
	})

	t.Run("manual", func(t *testing.T) {
		got := tl.ApprovalSummary(map[string]any{
			"name":    "once",
			"command": "/bin/true",
		})
		if !strings.Contains(got, "manual") {
			t.Fatalf("ApprovalSummary = %q, want it to mention manual", got)
		}
	})
}

func TestInvoke_StampsNewJobUnconfirmed(t *testing.T) {
	s, prov := newStore(t)
	tl := New(prov)

	if _, err := tl.Invoke(context.Background(), map[string]any{
		"name":    "held",
		"command": "/bin/echo",
		"every":   "1h",
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	job, ok := getJob(t, s, "held")
	if !ok {
		t.Fatal("job not stored")
	}
	if !job.AwaitingConfirmation() {
		t.Fatalf("job.AwaitingConfirmation() = false, want true (Confirmed=%v)", job.Confirmed)
	}
}

func TestInvoke_UpdatesExistingJob_PreservesOthers(t *testing.T) {
	s, prov := newStore(t)
	ctx := context.Background()
	if err := s.UpsertItem(ctx, config.SectionJob, "keep", config.JobProfile{Command: "/bin/true"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertItem(ctx, config.SectionJob, "edit", config.JobProfile{Command: "/bin/false"}); err != nil {
		t.Fatal(err)
	}
	tl := New(prov)

	res, err := tl.Invoke(ctx, map[string]any{"name": "edit", "command": "/bin/echo"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.(Result).Created {
		t.Fatal("Result.Created = true, want false on update")
	}
	if job, _ := getJob(t, s, "edit"); job.Command != "/bin/echo" {
		t.Fatalf("job[edit].Command = %q, want /bin/echo", job.Command)
	}
	if job, _ := getJob(t, s, "keep"); job.Command != "/bin/true" {
		t.Fatalf("job[keep] was clobbered: %+v", job)
	}
}

func TestInvoke_RejectsMissingFields(t *testing.T) {
	_, prov := newStore(t)
	tl := New(prov)
	cases := []map[string]any{
		{},
		{"name": "x"},
		{"command": "/bin/x"},
		{"name": "x", "command": "/bin/x", "timeout": "not-a-duration"},
	}
	for i, args := range cases {
		if _, err := tl.Invoke(context.Background(), args); err == nil {
			t.Fatalf("case %d: Invoke returned nil, want error for %+v", i, args)
		}
	}
}

func TestResult_String(t *testing.T) {
	r := Result{Name: "demo", Created: true}
	got := r.String()
	if !strings.Contains(got, "created") || !strings.Contains(got, "demo") {
		t.Fatalf("String() = %q, want it to mention created + demo", got)
	}
}
