package cfg

import (
	"context"
	"fmt"
	"os"

	"github.com/google/jsonschema-go/jsonschema"
	"gopkg.in/yaml.v3"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/tools"
)

// Workflow and unfurl rules carry polymorphic, nested trigger blocks
// (TriggerConfig has custom YAML/JSON codecs), so typed CLI flags cannot
// express them. Instead the operator authors the rule as YAML and points the
// set tool at the file; it is decoded into the typed struct and stored (the
// store marshals it to JSON, which round-trips through the trigger codecs).

// ruleSetTool loads one polymorphic rule from a YAML file and upserts it.
type ruleSetTool struct {
	p       Provider
	section string
	name    string
	label   string
	decode  func([]byte) (any, error)
}

func (t *ruleSetTool) Name() string { return t.name }
func (t *ruleSetTool) Description() string {
	return fmt.Sprintf("Create or update a %s from a YAML file (--name, --from-file).", t.label)
}
func (t *ruleSetTool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"name":      {Type: "string", Description: "the " + t.label + " name (the key it is stored under)"},
			"from_file": {Type: "string", Description: "path to a YAML file holding the " + t.label + " definition"},
		},
	}
}
func (t *ruleSetTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	name, err := requireString(args, "name")
	if err != nil {
		return nil, err
	}
	path, err := requireString(args, "from_file")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	rule, err := t.decode(data)
	if err != nil {
		return nil, fmt.Errorf("parse %s %q: %w", t.label, path, err)
	}
	s, err := t.p()
	if err != nil {
		return nil, err
	}
	if err := upsertItemValidated(ctx, s, t.section, name, rule); err != nil {
		return nil, err
	}
	return okResult{Message: fmt.Sprintf("saved %s %q", t.label, name)}, nil
}

// RuleTools returns the workflow-rule and unfurl-rule set tools plus each
// section's shared list/show/delete trio.
func RuleTools(p Provider) []tools.Tool {
	workflowSet := &ruleSetTool{
		p:       p,
		section: config.SectionWorkflowRule,
		name:    "cfg.workflow-rule.set",
		label:   "workflow rule",
		decode: func(data []byte) (any, error) {
			var rule config.WorkflowRuleConfig
			if err := yaml.Unmarshal(data, &rule); err != nil {
				return nil, err
			}
			return rule, nil
		},
	}
	unfurlSet := &ruleSetTool{
		p:       p,
		section: config.SectionUnfurlRule,
		name:    "cfg.unfurl-rule.set",
		label:   "unfurl rule",
		decode: func(data []byte) (any, error) {
			var rule config.UnfurlRuleConfig
			if err := yaml.Unmarshal(data, &rule); err != nil {
				return nil, err
			}
			return rule, nil
		},
	}
	out := []tools.Tool{workflowSet}
	out = append(out, sectionTools(p, config.SectionWorkflowRule, "cfg.workflow-rule", "workflow rule")...)
	out = append(out, unfurlSet)
	out = append(out, sectionTools(p, config.SectionUnfurlRule, "cfg.unfurl-rule", "unfurl rule")...)
	return out
}
