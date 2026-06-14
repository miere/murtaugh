package acp

import (
	"strings"
	"testing"
)

func TestPromptBlocksWithoutContext(t *testing.T) {
	blocks := promptBlocks(PromptRequest{Text: "hello"})
	if len(blocks) != 1 {
		t.Fatalf("expected a single text block when no conversation context is set, got %d", len(blocks))
	}
	if blocks[0]["type"] != "text" || blocks[0]["text"] != "hello" {
		t.Fatalf("unexpected block: %#v", blocks[0])
	}
}

func TestPromptBlocksPrependsConversationContext(t *testing.T) {
	blocks := promptBlocks(PromptRequest{Text: "please restart", Channel: "C123", Thread: "1699999999.000001"})
	if len(blocks) != 2 {
		t.Fatalf("expected a leading context block plus the user text, got %d", len(blocks))
	}
	ctx := blocks[0]["text"]
	if !strings.Contains(ctx, "C123") || !strings.Contains(ctx, "1699999999.000001") {
		t.Fatalf("context block should carry channel and thread, got %q", ctx)
	}
	if !strings.Contains(ctx, "restart") {
		t.Fatalf("context block should hint the restart tool, got %q", ctx)
	}
	// The user's own text must still be the final block, unaltered.
	if blocks[1]["text"] != "please restart" {
		t.Fatalf("expected user text preserved as last block, got %q", blocks[1]["text"])
	}
}

func TestPromptBlocksThreadOptional(t *testing.T) {
	// A channel without a thread (e.g. a channel-root chat) still injects context.
	blocks := promptBlocks(PromptRequest{Text: "hi", Channel: "C123"})
	if len(blocks) != 2 {
		t.Fatalf("expected context block even without a thread, got %d", len(blocks))
	}
}
