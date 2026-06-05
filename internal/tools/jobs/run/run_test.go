package run

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
)

func lookupFrom(jobs map[string]config.JobProfile) JobLookup {
	return func(name string) (config.JobProfile, bool) {
		j, ok := jobs[name]
		return j, ok
	}
}

func TestTool_Metadata(t *testing.T) {
	tl := New(lookupFrom(nil))
	if tl.Name() != "jobs.run" {
		t.Fatalf("Name() = %q, want jobs.run", tl.Name())
	}
	schema := tl.InputSchema()
	if schema == nil || schema.Type != "object" {
		t.Fatalf("InputSchema = %+v, want object", schema)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "name" {
		t.Fatalf("required = %v, want [name]", schema.Required)
	}
}

func TestInvoke_MissingName(t *testing.T) {
	tl := New(lookupFrom(nil))
	if _, err := tl.Invoke(context.Background(), map[string]any{}); err == nil {
		t.Fatal("Invoke returned nil, want error for missing name")
	}
}

func TestInvoke_UnknownJob(t *testing.T) {
	tl := New(lookupFrom(map[string]config.JobProfile{}))
	_, err := tl.Invoke(context.Background(), map[string]any{"name": "missing"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Invoke err = %v, want 'not found'", err)
	}
}

func TestInvoke_RunsCommand_CapturesStdout(t *testing.T) {
	jobs := map[string]config.JobProfile{
		"hello": {Command: "/bin/echo", Args: []string{"hello", "world"}},
	}
	var stdout, stderr bytes.Buffer
	tl := NewWith(lookupFrom(jobs), &stdout, &stderr)

	res, err := tl.Invoke(context.Background(), map[string]any{"name": "hello"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r, ok := res.(Result)
	if !ok {
		t.Fatalf("Invoke returned %T, want Result", res)
	}
	if r.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", r.ExitCode)
	}
	if !strings.Contains(r.Stdout, "hello world") {
		t.Fatalf("Result.Stdout = %q, want it to contain 'hello world'", r.Stdout)
	}
	if !strings.Contains(stdout.String(), "hello world") {
		t.Fatalf("captured stdout = %q, want it to contain 'hello world'", stdout.String())
	}
}

func TestInvoke_NonZeroExit_ReturnsResult(t *testing.T) {
	jobs := map[string]config.JobProfile{
		"fail": {Command: "/bin/sh", Args: []string{"-c", "exit 3"}},
	}
	var stdout, stderr bytes.Buffer
	tl := NewWith(lookupFrom(jobs), &stdout, &stderr)

	res, err := tl.Invoke(context.Background(), map[string]any{"name": "fail"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if r.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", r.ExitCode)
	}
}

func TestResult_String(t *testing.T) {
	r := Result{Name: "demo", ExitCode: 0}
	got := r.String()
	if !strings.Contains(got, "demo") || !strings.Contains(got, "exit 0") {
		t.Fatalf("String() = %q, want it to mention demo and exit 0", got)
	}
}
