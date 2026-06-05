// Package run implements the `jobs.run` tool: execute a job defined in
// jobs.yaml. The tool resolves the job by name against the loaded
// configuration, applies the per-job timeout (default 10 minutes), and runs
// the command with the configured args / workdir.
//
// Stdout and stderr from the executed process are streamed to the supplied
// writers — in the CLI frontend that is the user's terminal; the MCP
// frontend captures them so they appear in the JSON result.
package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
)

// defaultTimeout matches the previous hand-rolled subcommand: jobs without an
// explicit timeout run up to 10 minutes.
const defaultTimeout = 10 * time.Minute

// JobLookup returns the JobProfile registered under name, if any. The
// composition root supplies a closure over the loaded config.
type JobLookup func(name string) (config.JobProfile, bool)

// Tool is the `jobs.run` capability.
type Tool struct {
	lookup JobLookup
	stdout io.Writer
	stderr io.Writer
}

// New constructs a Tool that resolves jobs through lookup and streams the
// child process stdout/stderr to the calling process's stdout/stderr. The
// CLI frontend gets a live console; the MCP frontend wraps it with a Tool
// configured via NewWith to capture output into the result instead.
func New(lookup JobLookup) *Tool {
	return &Tool{lookup: lookup, stdout: os.Stdout, stderr: os.Stderr}
}

// NewWith returns a Tool whose stdout/stderr are redirected to the supplied
// writers. Intended for tests and for frontends that need to capture output.
func NewWith(lookup JobLookup, stdout, stderr io.Writer) *Tool {
	return &Tool{lookup: lookup, stdout: stdout, stderr: stderr}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "jobs.run" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Run a job defined in jobs.yaml by name."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name": {Type: "string", Description: "Name of the job as keyed in jobs.yaml."},
		},
		Required: []string{"name"},
	}
}

// Result is the structured payload returned by Invoke. The MCP frontend
// JSON-marshals it; the CLI frontend renders it via String().
type Result struct {
	Name     string `json:"name"`
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
}

// String renders a one-line CLI confirmation.
func (r Result) String() string {
	return fmt.Sprintf("job %q completed (exit %d)", r.Name, r.ExitCode)
}

// Invoke resolves the named job and runs it. The job's configured timeout
// (default 10m) bounds the execution; stdout/stderr from the child process
// are streamed to the tool's writers and also captured into Result so the
// MCP frontend can surface them.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	name, _ := args["name"].(string)
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("name is required")
	}

	job, ok := t.lookup(name)
	if !ok {
		return nil, fmt.Errorf("job %q not found in jobs.yaml", name)
	}
	if strings.TrimSpace(job.Command) == "" {
		return nil, fmt.Errorf("job %q has no command configured", name)
	}

	timeout := defaultTimeout
	if job.Timeout != "" {
		if d, err := time.ParseDuration(job.Timeout); err == nil {
			timeout = d
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd := exec.CommandContext(runCtx, job.Command, job.Args...)
	cmd.Dir = job.WorkDir
	cmd.Stdin = bytes.NewReader(nil)
	cmd.Stdout = io.MultiWriter(t.stdout, &stdoutBuf)
	cmd.Stderr = io.MultiWriter(t.stderr, &stderrBuf)

	err := cmd.Run()
	exit := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exit = exitErr.ExitCode()
		} else {
			return nil, fmt.Errorf("job %q: %w", name, err)
		}
	}
	return Result{
		Name:     name,
		Command:  job.Command,
		ExitCode: exit,
		Stdout:   stdoutBuf.String(),
		Stderr:   stderrBuf.String(),
	}, nil
}
