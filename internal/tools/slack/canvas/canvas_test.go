package canvas

import (
	"context"
	"testing"

	"github.com/miere/murtaugh/internal/slack/client/slacktest"
)

func TestTool_Metadata(t *testing.T) {
	tool := New("")
	if tool.Name() != "slack.canvas" {
		t.Fatalf("Name = %q, want slack.canvas", tool.Name())
	}
	schema := tool.InputSchema()
	if schema == nil || schema.Type != "object" {
		t.Fatalf("InputSchema = %+v, want object schema", schema)
	}
	req := map[string]bool{}
	for _, r := range schema.Required {
		req[r] = true
	}
	if !req["action"] || !req["canvas_id"] {
		t.Fatalf("required must include action and canvas_id, got %v", schema.Required)
	}
}

func TestInvoke_ReadReturnsContent(t *testing.T) {
	fake := &slacktest.FakeAPI{CanvasContent: "# Spec\nhello"}
	tool := NewWith(fake.LazyClient())

	res, err := tool.Invoke(context.Background(), map[string]any{"action": "read", "canvas_id": "F1"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if !r.OK || r.Content != "# Spec\nhello" {
		t.Fatalf("Result = %+v, want the canvas content", r)
	}
	if r.String() != "# Spec\nhello" {
		t.Fatalf("read String() should be the content, got %q", r.String())
	}
	if len(fake.ReadCanvasIDs) != 1 || fake.ReadCanvasIDs[0] != "F1" {
		t.Fatalf("ReadCanvasIDs = %v, want [F1]", fake.ReadCanvasIDs)
	}
}

func TestInvoke_EditPageAppendsByDefault(t *testing.T) {
	fake := &slacktest.FakeAPI{}
	tool := NewWith(fake.LazyClient())

	if _, err := tool.Invoke(context.Background(), map[string]any{
		"action": "edit_page", "canvas_id": "F1", "markdown": "more",
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fake.CanvasEdits) != 1 {
		t.Fatalf("expected one edit, got %d", len(fake.CanvasEdits))
	}
	e := fake.CanvasEdits[0]
	if e.CanvasID != "F1" || e.Operation != "insert_at_end" || e.Markdown != "more" || e.SectionID != "" {
		t.Fatalf("edit = %+v, want append (insert_at_end) with no section", e)
	}
}

func TestInvoke_EditPagePrepend(t *testing.T) {
	fake := &slacktest.FakeAPI{}
	tool := NewWith(fake.LazyClient())
	if _, err := tool.Invoke(context.Background(), map[string]any{
		"action": "edit_page", "canvas_id": "F1", "markdown": "top", "operation": "prepend",
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if fake.CanvasEdits[0].Operation != "insert_at_start" {
		t.Fatalf("prepend should map to insert_at_start, got %q", fake.CanvasEdits[0].Operation)
	}
}

func TestInvoke_EditSectionLooksUpThenReplaces(t *testing.T) {
	fake := &slacktest.FakeAPI{SectionID: "sec-42"}
	tool := NewWith(fake.LazyClient())

	if _, err := tool.Invoke(context.Background(), map[string]any{
		"action": "edit_section", "canvas_id": "F1", "section_contains": "Overview", "markdown": "new body",
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(fake.LookupSectionArgs) != 1 || fake.LookupSectionArgs[0].ContainsText != "Overview" {
		t.Fatalf("LookupSectionArgs = %+v, want a lookup for 'Overview'", fake.LookupSectionArgs)
	}
	e := fake.CanvasEdits[0]
	if e.Operation != "replace" || e.SectionID != "sec-42" || e.Markdown != "new body" {
		t.Fatalf("edit = %+v, want replace of sec-42", e)
	}
}

func TestInvoke_EditSectionDeleteNeedsNoMarkdown(t *testing.T) {
	fake := &slacktest.FakeAPI{SectionID: "sec-1"}
	tool := NewWith(fake.LazyClient())
	if _, err := tool.Invoke(context.Background(), map[string]any{
		"action": "edit_section", "canvas_id": "F1", "section_contains": "old", "operation": "delete",
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if fake.CanvasEdits[0].Operation != "delete" || fake.CanvasEdits[0].SectionID != "sec-1" {
		t.Fatalf("edit = %+v, want delete of sec-1", fake.CanvasEdits[0])
	}
}

func TestInvoke_EditSectionNoMatchErrors(t *testing.T) {
	fake := &slacktest.FakeAPI{SectionID: ""} // no section matched
	tool := NewWith(fake.LazyClient())
	if _, err := tool.Invoke(context.Background(), map[string]any{
		"action": "edit_section", "canvas_id": "F1", "section_contains": "nope", "markdown": "x",
	}); err == nil {
		t.Fatal("expected an error when no section matches")
	}
	if len(fake.CanvasEdits) != 0 {
		t.Fatalf("must not edit when no section matched, got %+v", fake.CanvasEdits)
	}
}

func TestInvoke_Validation(t *testing.T) {
	fake := &slacktest.FakeAPI{}
	tool := NewWith(fake.LazyClient())
	cases := []map[string]any{
		{"action": "read"},                         // missing canvas_id
		{"action": "edit_page", "canvas_id": "F1"}, // missing markdown
		{"action": "edit_page", "canvas_id": "F1", "markdown": "x", "operation": "bogus"}, // bad op
		{"action": "edit_section", "canvas_id": "F1", "markdown": "x"},                    // missing section_contains
		{"action": "bogus", "canvas_id": "F1"},                                            // bad action
	}
	for i, args := range cases {
		if _, err := tool.Invoke(context.Background(), args); err == nil {
			t.Fatalf("case %d %v: expected an error", i, args)
		}
	}
	if len(fake.CanvasEdits) != 0 || len(fake.ReadCanvasIDs) != 0 {
		t.Fatalf("validation failures must not call Slack: edits=%v reads=%v", fake.CanvasEdits, fake.ReadCanvasIDs)
	}
}
