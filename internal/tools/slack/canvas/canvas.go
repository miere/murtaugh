// Package canvas implements the `slack.canvas` tool: read or edit a Slack canvas
// document. A canvas mention seeds the agent with the canvas id (spec 021 §9.3),
// and this tool lets it act on that document — or on any canvas by id, e.g. to
// read a spec a teammate wrote (spec 021 §9.4).
package canvas

import (
	"context"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"

	slacklib "github.com/miere/murtaugh/internal/slack/client"
)

// Tool is the `slack.canvas` capability.
type Tool struct {
	client *slacklib.LazyClient
}

// New constructs a Tool that builds its Slack client lazily from botToken
// (oauth.bot_token in config.yaml).
func New(botToken string) *Tool {
	return &Tool{client: slacklib.NewLazyClient(botToken)}
}

// NewWith constructs a Tool against the given LazyClient. Intended for tests so
// they can inject a fake SlackAPI.
func NewWith(client *slacklib.LazyClient) *Tool {
	return &Tool{client: client}
}

func (t *Tool) Name() string { return "slack.canvas" }

func (t *Tool) Description() string {
	return "Read or edit a Slack canvas document by its canvas id (F…). Reads and writes both use Markdown. " +
		"Actions: read (return the whole page as Markdown); edit_page (append/prepend Markdown to the page); " +
		"edit_section (replace, insert around, or delete the section matching some text)."
}

func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:     "object",
		Required: []string{"action", "canvas_id"},
		Properties: map[string]*jsonschema.Schema{
			"action": {
				Type:        "string",
				Enum:        []any{"read", "edit_page", "edit_section"},
				Description: "read the whole canvas, edit_page (append/prepend), or edit_section (target a section by its text).",
			},
			"canvas_id": {Type: "string", Description: "The canvas id (F…). For a canvas mention it is provided in the turn context."},
			"markdown":  {Type: "string", Description: "Markdown content. Required for edit_page and for edit_section unless operation=delete."},
			"operation": {
				Type: "string",
				Enum: []any{"append", "prepend", "replace", "insert_after", "insert_before", "delete"},
				Description: "edit_page: append (default) or prepend. " +
					"edit_section: replace (default), insert_after, insert_before, or delete.",
			},
			"section_contains": {Type: "string", Description: "edit_section only: text identifying the section to edit (the first matching section is used)."},
		},
	}
}

func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	action, _ := args["action"].(string)
	canvasID, _ := args["canvas_id"].(string)
	markdown, _ := args["markdown"].(string)
	operation, _ := args["operation"].(string)
	sectionContains, _ := args["section_contains"].(string)

	if canvasID == "" {
		return nil, fmt.Errorf("Error: --canvas_id is required")
	}
	api, err := t.client.Get()
	if err != nil {
		return nil, err
	}

	switch action {
	case "read":
		return t.read(ctx, api, canvasID)
	case "edit_page":
		return t.editPage(ctx, api, canvasID, operation, markdown)
	case "edit_section":
		return t.editSection(ctx, api, canvasID, operation, sectionContains, markdown)
	case "":
		return nil, fmt.Errorf("Error: --action is required (read, edit_page, or edit_section)")
	default:
		return nil, fmt.Errorf("Error: unknown --action %q (want read, edit_page, or edit_section)", action)
	}
}

func (t *Tool) read(ctx context.Context, api slacklib.SlackAPI, canvasID string) (any, error) {
	content, err := api.ReadCanvas(ctx, canvasID)
	if err != nil {
		return nil, err
	}
	return Result{OK: true, Action: "read", CanvasID: canvasID, Content: content}, nil
}

func (t *Tool) editPage(ctx context.Context, api slacklib.SlackAPI, canvasID, operation, markdown string) (any, error) {
	if markdown == "" {
		return nil, fmt.Errorf("Error: --markdown is required for edit_page")
	}
	op := "insert_at_end"
	switch operation {
	case "", "append":
		op = "insert_at_end"
	case "prepend":
		op = "insert_at_start"
	default:
		return nil, fmt.Errorf("Error: edit_page --operation must be append or prepend, got %q", operation)
	}
	if err := api.EditCanvas(ctx, slacklib.CanvasEditParams{CanvasID: canvasID, Operation: op, Markdown: markdown}); err != nil {
		return nil, err
	}
	return Result{OK: true, Action: "edit_page", CanvasID: canvasID, Detail: fmt.Sprintf("Canvas %s updated (%s).", canvasID, op)}, nil
}

func (t *Tool) editSection(ctx context.Context, api slacklib.SlackAPI, canvasID, operation, sectionContains, markdown string) (any, error) {
	if sectionContains == "" {
		return nil, fmt.Errorf("Error: --section_contains is required for edit_section")
	}
	op := "replace"
	switch operation {
	case "", "replace":
		op = "replace"
	case "insert_after", "insert_before", "delete":
		op = operation
	default:
		return nil, fmt.Errorf("Error: edit_section --operation must be replace, insert_after, insert_before, or delete, got %q", operation)
	}
	if op != "delete" && markdown == "" {
		return nil, fmt.Errorf("Error: --markdown is required for edit_section unless operation=delete")
	}
	sectionID, err := api.LookupCanvasSection(ctx, canvasID, sectionContains)
	if err != nil {
		return nil, err
	}
	if sectionID == "" {
		return nil, fmt.Errorf("Error: no canvas section matched %q", sectionContains)
	}
	if err := api.EditCanvas(ctx, slacklib.CanvasEditParams{CanvasID: canvasID, Operation: op, SectionID: sectionID, Markdown: markdown}); err != nil {
		return nil, err
	}
	return Result{OK: true, Action: "edit_section", CanvasID: canvasID, Detail: fmt.Sprintf("Canvas %s section %s updated (%s).", canvasID, sectionID, op)}, nil
}

// Result is the structured outcome of a canvas action.
type Result struct {
	OK       bool   `json:"ok"`
	Action   string `json:"action"`
	CanvasID string `json:"canvasId"`
	Content  string `json:"content,omitempty"` // read: the canvas text
	Detail   string `json:"detail,omitempty"`  // edit_*: what happened
}

func (r Result) String() string {
	if r.Action == "read" {
		return r.Content
	}
	return r.Detail
}
