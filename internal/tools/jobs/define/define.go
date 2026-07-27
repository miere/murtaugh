// Package define implements the `jobs.define` tool: register a new job (or
// update an existing one) in the configuration store. Existing jobs that are
// not touched are preserved verbatim.
//
// The tool is deliberately small and does NOT execute the job — it only
// defines it. Use `jobs.run` to invoke a defined job.
package define

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/config"
)

// StoreProvider yields the open configuration store. The composition root
// supplies a closure; it is invoked per-call and may return an error when the
// store is unavailable.
type StoreProvider func() (config.Store, error)

// Tool is the `jobs.define` capability.
type Tool struct {
	store StoreProvider
}

// New constructs a Tool that writes jobs into the store returned by provider.
func New(provider StoreProvider) *Tool {
	return &Tool{store: provider}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "jobs.define" }

// RequiresApproval satisfies tools.ApprovalClassifier: defining a job always
// needs human approval. A defined job's command is later run HEADLESS by the
// scheduler/jobs.run path, so the approval at definition time is the only gate
// the human gets — it must never be skipped.
func (t *Tool) RequiresApproval(map[string]any) bool { return true }

// ApprovalSummary satisfies tools.ApprovalSummarizer: render the actual command
// and schedule the human is approving, rather than the gate's generic args
// rendering. The line reads e.g. `define job "nightly": runs "/bin/backup --all"
// — cron 0 2 * * *`.
func (t *Tool) ApprovalSummary(args map[string]any) string {
	name, _ := args["name"].(string)
	command, _ := args["command"].(string)
	schedule, _ := args["schedule"].(string)
	every, _ := args["every"].(string)
	jobArgs, _ := stringSlice(args["args"])

	cmd := strings.TrimSpace(command)
	if len(jobArgs) > 0 {
		cmd = strings.TrimSpace(cmd + " " + strings.Join(jobArgs, " "))
	}

	var when string
	switch {
	case strings.TrimSpace(every) != "":
		when = "every " + strings.TrimSpace(every)
	case strings.TrimSpace(schedule) != "":
		when = "cron " + strings.TrimSpace(schedule)
	default:
		when = "manual (no schedule)"
	}

	return fmt.Sprintf("define job %q: runs %q — %s", strings.TrimSpace(name), cmd, when)
}

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Register a job (command, args, workdir, timeout, schedule/every) in jobs.yaml."
}

// InputSchema returns the JSON Schema for the tool's arguments. `args` is a
// string of space-separated tokens, the same shape the CLI frontend hands
// over today; richer array handling can land later once tools share an
// argv-style parser.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name":     {Type: "string", Description: "Job key. Must be non-empty."},
			"command":  {Type: "string", Description: "Absolute path or PATH-resolved binary to execute."},
			"args":     {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "Positional arguments passed to command."},
			"workdir":  {Type: "string", Description: "Optional working directory."},
			"timeout":  {Type: "string", Description: "Optional Go duration (e.g. '5m'). Defaults to 10m at run-time when empty."},
			"schedule": {Type: "string", Description: "Optional 5-field cron expression (e.g. '0 2 * * *') to run the job automatically. Mutually exclusive with every."},
			"every":    {Type: "string", Description: "Optional Go duration (e.g. '1h') to run the job at a fixed interval. Mutually exclusive with schedule."},
		},
		Required: []string{"name", "command"},
	}
}

// Result is the structured payload returned by Invoke.
type Result struct {
	Name    string            `json:"name"`
	Created bool              `json:"created"`
	Job     config.JobProfile `json:"job"`
}

// String renders a one-line CLI confirmation.
func (r Result) String() string {
	verb := "updated"
	if r.Created {
		verb = "created"
	}
	return fmt.Sprintf("%s job %q in the config store", verb, r.Name)
}

// Invoke validates the arguments and writes the job into the configuration
// store. Existing jobs are preserved; the entry is stamped unconfirmed so the
// scheduler holds an agent-defined job back until a human confirms it.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	name, _ := args["name"].(string)
	command, _ := args["command"].(string)
	workdir, _ := args["workdir"].(string)
	timeout, _ := args["timeout"].(string)
	schedule, _ := args["schedule"].(string)
	every, _ := args["every"].(string)

	if strings.TrimSpace(name) == "" {
		return nil, errors.New("name is required")
	}
	if strings.TrimSpace(command) == "" {
		return nil, errors.New("command is required")
	}
	if timeout != "" {
		if _, err := time.ParseDuration(timeout); err != nil {
			return nil, fmt.Errorf("timeout %q is not a valid duration: %w", timeout, err)
		}
	}
	if strings.TrimSpace(schedule) != "" && strings.TrimSpace(every) != "" {
		return nil, errors.New("schedule and every are mutually exclusive; set at most one")
	}
	if e := strings.TrimSpace(every); e != "" {
		if d, err := time.ParseDuration(e); err != nil {
			return nil, fmt.Errorf("every %q is not a valid duration: %w", every, err)
		} else if d <= 0 {
			return nil, fmt.Errorf("every %q must be greater than zero", every)
		}
	}

	jobArgs, err := stringSlice(args["args"])
	if err != nil {
		return nil, fmt.Errorf("args: %w", err)
	}

	s, err := t.store()
	if err != nil {
		return nil, err
	}
	_, exists, err := s.GetItem(ctx, config.SectionJob, name)
	if err != nil {
		return nil, err
	}

	// Stamp agent-defined jobs as unconfirmed so the scheduler holds them back
	// from auto-running until a human confirms the first run. This closes the
	// bypass where an agent could define a scheduled command that then runs
	// headless and ungated.
	unconfirmed := false
	job := config.JobProfile{
		Command:   command,
		Args:      jobArgs,
		WorkDir:   workdir,
		Timeout:   timeout,
		Schedule:  schedule,
		Every:     every,
		Confirmed: &unconfirmed,
	}
	if err := s.UpsertItem(ctx, config.SectionJob, name, job); err != nil {
		return nil, err
	}
	return Result{Name: name, Created: !exists, Job: job}, nil
}
