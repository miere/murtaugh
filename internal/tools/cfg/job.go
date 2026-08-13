package cfg

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/tools"
)

// jobSetTool creates or updates one job from typed flags. A job is either a
// plain command (--command/--arg) or an agent delegation (--agent/--prompt),
// optionally scheduled (--schedule cron or --every duration); Validate enforces
// the exclusivity, so upsertItemValidated rejects an invalid combination.
//
// Every write stamps the entry unconfirmed, so a modified job is held for
// re-approval before its next scheduled run (see Invoke).
type jobSetTool struct{ p Provider }

func (t *jobSetTool) Name() string { return "cfg.job.set" }
func (t *jobSetTool) Description() string {
	return "Create or update a job (e.g. `cfg job set --name nightly --command ./backup.sh --schedule '0 2 * * *'`). Any change re-arms first-run approval: the scheduler holds the job until the admin confirms it again."
}
func (t *jobSetTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name":     {Type: "string", Description: "job name (the key it is stored under)"},
			"command":  {Type: "string", Description: "command to run (mutually exclusive with agent/prompt)"},
			"arg":      {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "command argument (repeatable)"},
			"workdir":  {Type: "string", Description: "working directory for the command"},
			"timeout":  {Type: "string", Description: "run timeout as a Go duration (e.g. 30m)"},
			"agent":    {Type: "string", Description: "delegate to this agent instead of running a command"},
			"prompt":   {Type: "string", Description: "prompt sent to the delegated agent (supports {{ 1 }} placeholders)"},
			"schedule": {Type: "string", Description: "cron schedule, 5-field (mutually exclusive with every)"},
			"every":    {Type: "string", Description: "fixed interval as a Go duration (mutually exclusive with schedule)"},
		},
	}
}
func (t *jobSetTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	name, err := requireString(args, "name")
	if err != nil {
		return nil, err
	}
	s, err := t.p()
	if err != nil {
		return nil, err
	}
	var job config.JobProfile
	if body, ok, err := s.GetItem(ctx, config.SectionJob, name); err != nil {
		return nil, err
	} else if ok {
		if err := json.Unmarshal(body, &job); err != nil {
			return nil, err
		}
	}
	if v, ok := stringArg(args, "command"); ok {
		job.Command = v
	}
	if v, ok := arrayArg(args, "arg"); ok {
		job.Args = v
	}
	if v, ok := stringArg(args, "workdir"); ok {
		job.WorkDir = v
	}
	if v, ok := stringArg(args, "timeout"); ok {
		job.Timeout = v
	}
	if v, ok := stringArg(args, "agent"); ok {
		job.Agent = v
	}
	if v, ok := stringArg(args, "prompt"); ok {
		job.Prompt = v
	}
	if v, ok := stringArg(args, "schedule"); ok {
		job.Schedule = v
	}
	if v, ok := stringArg(args, "every"); ok {
		job.Every = v
	}
	// Re-arm the first-run gate on every modification. A job's approval is
	// granted against a specific command and schedule, so any change to the
	// entry invalidates it and the scheduler must ask again before running.
	// jobs.define stamps the same mark, so both write surfaces (CLI and MCP)
	// behave identically no matter who edited the job.
	unconfirmed := false
	job.Confirmed = &unconfirmed
	if err := upsertItemValidated(ctx, s, config.SectionJob, name, job); err != nil {
		return nil, err
	}
	return okResult{Message: fmt.Sprintf("saved job %q", name)}, nil
}

// JobTools returns the job set tool plus the shared list/show/delete trio.
func JobTools(p Provider) []tools.Tool {
	out := []tools.Tool{&jobSetTool{p: p}}
	return append(out, sectionTools(p, config.SectionJob, "cfg.job", "job")...)
}
