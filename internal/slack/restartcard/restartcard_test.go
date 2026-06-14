package restartcard

import (
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

func TestBuildShape(t *testing.T) {
	blocks := Build("config changed at /etc/murtaugh.yaml")
	if len(blocks) != 3 {
		t.Fatalf("expected 3 blocks (section, context, actions), got %d", len(blocks))
	}
	actions, ok := blocks[2].(*slack.ActionBlock)
	if !ok {
		t.Fatalf("expected blocks[2] to be ActionBlock, got %T", blocks[2])
	}
	if actions.BlockID != BlockID {
		t.Fatalf("expected block_id %q, got %q", BlockID, actions.BlockID)
	}
	if actions.Elements == nil || len(actions.Elements.ElementSet) != 2 {
		t.Fatalf("expected 2 action elements, got %#v", actions.Elements)
	}
	confirm, ok := actions.Elements.ElementSet[0].(*slack.ButtonBlockElement)
	if !ok || confirm.ActionID != ActionConfirm {
		t.Fatalf("expected first element to be confirm button, got %#v", actions.Elements.ElementSet[0])
	}
	if confirm.Style != slack.StylePrimary {
		t.Fatalf("expected confirm button to be primary-styled, got %q", confirm.Style)
	}
	if !strings.Contains(confirm.Value, "config changed") {
		t.Fatalf("expected reason embedded in confirm button value, got %q", confirm.Value)
	}
	dismiss, ok := actions.Elements.ElementSet[1].(*slack.ButtonBlockElement)
	if !ok || dismiss.ActionID != ActionDismiss {
		t.Fatalf("expected second element to be dismiss button, got %#v", actions.Elements.ElementSet[1])
	}
}

func TestBuildDefaultsBlankReason(t *testing.T) {
	blocks := Build("   ")
	context, ok := blocks[1].(*slack.ContextBlock)
	if !ok {
		t.Fatalf("expected blocks[1] to be ContextBlock, got %T", blocks[1])
	}
	if len(context.ContextElements.Elements) == 0 {
		t.Fatal("expected context block to carry the default reason")
	}
	text, ok := context.ContextElements.Elements[0].(*slack.TextBlockObject)
	if !ok || !strings.Contains(text.Text, "restart") {
		t.Fatalf("expected default reason mentioning restart, got %#v", context.ContextElements.Elements[0])
	}
}
