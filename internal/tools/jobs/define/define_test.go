package define

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
)

func loadJobs(t *testing.T, path string) map[string]config.JobProfile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Jobs map[string]config.JobProfile `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc.Jobs
}

func TestTool_Metadata(t *testing.T) {
	tl := New(func() string { return "" })
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
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.yaml")
	tl := New(func() string { return path })

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
	jobs := loadJobs(t, path)
	if jobs["hello"].Command != "/bin/echo" {
		t.Fatalf("jobs[hello].Command = %q, want /bin/echo", jobs["hello"].Command)
	}
	if len(jobs["hello"].Args) != 1 || jobs["hello"].Args[0] != "hi" {
		t.Fatalf("jobs[hello].Args = %v, want [hi]", jobs["hello"].Args)
	}
}

func TestInvoke_UpdatesExistingJob_PreservesOthers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.yaml")
	seed := []byte("jobs:\n  keep:\n    command: /bin/true\n  edit:\n    command: /bin/false\n")
	if err := os.WriteFile(path, seed, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tl := New(func() string { return path })

	res, err := tl.Invoke(context.Background(), map[string]any{
		"name":    "edit",
		"command": "/bin/echo",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if r.Created {
		t.Fatal("Result.Created = true, want false on update")
	}
	jobs := loadJobs(t, path)
	if jobs["edit"].Command != "/bin/echo" {
		t.Fatalf("jobs[edit].Command = %q, want /bin/echo", jobs["edit"].Command)
	}
	if jobs["keep"].Command != "/bin/true" {
		t.Fatalf("jobs[keep] was clobbered: %+v", jobs["keep"])
	}
}

func TestInvoke_RejectsMissingFields(t *testing.T) {
	tl := New(func() string { return filepath.Join(t.TempDir(), "jobs.yaml") })
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
	r := Result{Name: "demo", Path: "/tmp/jobs.yaml", Created: true}
	got := r.String()
	if !strings.Contains(got, "created") || !strings.Contains(got, "demo") {
		t.Fatalf("String() = %q, want it to mention created + demo", got)
	}
}
