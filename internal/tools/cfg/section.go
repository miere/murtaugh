package cfg

import (
	"context"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/tools"
)

// The list/show/delete verbs are identical across every collection section
// (agent, mcp, job, workflow-rule, unfurl-rule), so they are implemented once
// here and instantiated per section. Only create/update carry section-specific
// typed flags and live in their own files.

// nameSchema is the shared single-argument schema for show/delete.
func nameSchema(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:       "object",
		Properties: map[string]*jsonschema.Schema{"name": {Type: "string", Description: desc}},
	}
}

// listTool lists the entity names in one section.
type listTool struct {
	p       Provider
	section string
	name    string
	label   string
}

func newListTool(p Provider, section, toolName, label string) *listTool {
	return &listTool{p: p, section: section, name: toolName, label: label}
}

func (t *listTool) Name() string                    { return t.name }
func (t *listTool) Description() string             { return fmt.Sprintf("List configured %s.", t.label) }
func (t *listTool) InputSchema() *jsonschema.Schema { return nil }
func (t *listTool) Invoke(ctx context.Context, _ map[string]any) (any, error) {
	s, err := t.p()
	if err != nil {
		return nil, err
	}
	rows, err := s.ListItems(ctx, t.section)
	if err != nil {
		return nil, err
	}
	return listResult{Section: t.label, Names: sortedNames(rows)}, nil
}

// showTool prints one entity's stored JSON body.
type showTool struct {
	p       Provider
	section string
	name    string
	label   string
}

func newShowTool(p Provider, section, toolName, label string) *showTool {
	return &showTool{p: p, section: section, name: toolName, label: label}
}

func (t *showTool) Name() string { return t.name }
func (t *showTool) Description() string {
	return fmt.Sprintf("Show one %s's configuration by name.", t.label)
}
func (t *showTool) InputSchema() *jsonschema.Schema { return nameSchema("the " + t.label + " name") }
func (t *showTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	name, err := requireString(args, "name")
	if err != nil {
		return nil, err
	}
	s, err := t.p()
	if err != nil {
		return nil, err
	}
	body, ok, err := s.GetItem(ctx, t.section, name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%s %q not found", t.label, name)
	}
	return showResult{Name: name, Body: body}, nil
}

// deleteTool removes one entity, re-validating so a delete that would orphan a
// reference (e.g. removing an agent a chat default points at) is rejected.
type deleteTool struct {
	p       Provider
	section string
	name    string
	label   string
}

func newDeleteTool(p Provider, section, toolName, label string) *deleteTool {
	return &deleteTool{p: p, section: section, name: toolName, label: label}
}

func (t *deleteTool) Name() string { return t.name }
func (t *deleteTool) Description() string {
	return fmt.Sprintf("Delete one %s by name.", t.label)
}
func (t *deleteTool) InputSchema() *jsonschema.Schema { return nameSchema("the " + t.label + " name") }
func (t *deleteTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	name, err := requireString(args, "name")
	if err != nil {
		return nil, err
	}
	s, err := t.p()
	if err != nil {
		return nil, err
	}
	prior, existed, err := s.GetItem(ctx, t.section, name)
	if err != nil {
		return nil, err
	}
	if !existed {
		return nil, fmt.Errorf("%s %q not found", t.label, name)
	}
	if _, err := s.DeleteItem(ctx, t.section, name); err != nil {
		return nil, err
	}
	if verr := validateStore(ctx, s); verr != nil {
		_ = s.UpsertItem(ctx, t.section, name, prior)
		return nil, fmt.Errorf("delete rejected — config would be invalid: %w", verr)
	}
	return okResult{Message: fmt.Sprintf("deleted %s %q", t.label, name)}, nil
}

// sectionTools returns the standard list/show/delete trio for a section.
func sectionTools(p Provider, section, nsPrefix, label string) []tools.Tool {
	return []tools.Tool{
		newListTool(p, section, nsPrefix+".list", label),
		newShowTool(p, section, nsPrefix+".show", label),
		newDeleteTool(p, section, nsPrefix+".delete", label),
	}
}

var _ tools.Tool = (*listTool)(nil)
