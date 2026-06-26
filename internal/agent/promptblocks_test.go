package agent

import (
	"strings"
	"testing"
	"time"
)

// zeroContextClient returns a ProcessClient whose volatile <context> block is
// empty (zero clock, cwd "."), so the conversation-context/history/text
// behaviour can be asserted without the leading context block shifting counts.
func zeroContextClient() *ProcessClient {
	return &ProcessClient{now: func() time.Time { return time.Time{} }, opts: ProcessOptions{WorkDir: "."}}
}

// clockClient returns a ProcessClient with a fixed clock and working directory,
// so the volatile <context> block renders deterministically.
func clockClient(now time.Time, workDir string) *ProcessClient {
	return &ProcessClient{now: func() time.Time { return now }, opts: ProcessOptions{WorkDir: workDir}}
}

func TestPromptBlocksRendersVolatileContext(t *testing.T) {
	now := time.Date(2026, 6, 26, 14, 30, 0, 0, time.UTC)
	blocks := clockClient(now, "/work").promptBlocks(PromptRequest{Text: "hello"})
	if len(blocks) != 2 {
		t.Fatalf("expected a leading <context> block plus the user text, got %d", len(blocks))
	}
	ctx := blocks[0]["text"]
	if !strings.Contains(ctx, "<context>") || !strings.Contains(ctx, "2026-06-26 14:30") {
		t.Fatalf("context block should carry the current time, got %q", ctx)
	}
	if !strings.Contains(ctx, "Working directory: /work") {
		t.Fatalf("context block should carry the working directory, got %q", ctx)
	}
	if blocks[1]["text"] != "hello" {
		t.Fatalf("expected user text preserved as last block, got %q", blocks[1]["text"])
	}
}

func TestPromptBlocksInjectsPersona(t *testing.T) {
	c := zeroContextClient()
	c.opts.Persona = "You are Murtaugh."
	blocks := c.promptBlocks(PromptRequest{Text: "hi"})
	if len(blocks) != 2 {
		t.Fatalf("expected a leading persona block plus the user text, got %d", len(blocks))
	}
	if want := "<persona>\nYou are Murtaugh.\n</persona>"; blocks[0]["text"] != want {
		t.Fatalf("first block = %q, want the persona block %q", blocks[0]["text"], want)
	}
	if blocks[1]["text"] != "hi" {
		t.Fatalf("user text must remain last, got %q", blocks[1]["text"])
	}
}

func TestPromptBlocksPersonaLeadsContext(t *testing.T) {
	now := time.Date(2026, 6, 26, 14, 30, 0, 0, time.UTC)
	c := clockClient(now, "/work")
	c.opts.Persona = "Be Murtaugh."
	blocks := c.promptBlocks(PromptRequest{Text: "go", Channel: "C1"})
	// persona, context, conversation-context, text
	if len(blocks) != 4 {
		t.Fatalf("expected persona, context, conversation-context, text, got %d", len(blocks))
	}
	if !strings.Contains(blocks[0]["text"], "<persona>") {
		t.Fatalf("persona must lead, got %q", blocks[0]["text"])
	}
	if !strings.Contains(blocks[1]["text"], "<context>") {
		t.Fatalf("context must follow persona, got %q", blocks[1]["text"])
	}
}

func TestPromptBlocksWithoutVolatileContext(t *testing.T) {
	blocks := zeroContextClient().promptBlocks(PromptRequest{Text: "hello"})
	if len(blocks) != 1 {
		t.Fatalf("expected a single text block when nothing volatile to render, got %d", len(blocks))
	}
	if blocks[0]["type"] != "text" || blocks[0]["text"] != "hello" {
		t.Fatalf("unexpected block: %#v", blocks[0])
	}
}

func TestPromptBlocksPrependsConversationContext(t *testing.T) {
	blocks := zeroContextClient().promptBlocks(PromptRequest{Text: "please restart", Channel: "C123", Thread: "1699999999.000001"})
	if len(blocks) != 2 {
		t.Fatalf("expected a leading conversation-context block plus the user text, got %d", len(blocks))
	}
	ctx := blocks[0]["text"]
	if !strings.Contains(ctx, "C123") || !strings.Contains(ctx, "1699999999.000001") {
		t.Fatalf("context block should carry channel and thread, got %q", ctx)
	}
	if !strings.Contains(ctx, "restart") {
		t.Fatalf("context block should hint the restart tool, got %q", ctx)
	}
	if blocks[1]["text"] != "please restart" {
		t.Fatalf("expected user text preserved as last block, got %q", blocks[1]["text"])
	}
}

func TestPromptBlocksThreadOptional(t *testing.T) {
	// A channel without a thread (e.g. a channel-root chat) still injects context.
	blocks := zeroContextClient().promptBlocks(PromptRequest{Text: "hi", Channel: "C123"})
	if len(blocks) != 2 {
		t.Fatalf("expected context block even without a thread, got %d", len(blocks))
	}
}

func TestPromptBlocksInsertsHistoryBetweenContextAndText(t *testing.T) {
	blocks := zeroContextClient().promptBlocks(PromptRequest{
		Text:    "what's next?",
		Channel: "C123",
		Thread:  "1699999999.000001",
		History: "<thread-transcript>...</thread-transcript>",
	})
	if len(blocks) != 3 {
		t.Fatalf("expected context, history, and user text blocks, got %d", len(blocks))
	}
	if !strings.Contains(blocks[0]["text"], "C123") {
		t.Fatalf("first block should be the conversation context, got %q", blocks[0]["text"])
	}
	if blocks[1]["text"] != "<thread-transcript>...</thread-transcript>" {
		t.Fatalf("history block should be emitted verbatim, got %q", blocks[1]["text"])
	}
	if blocks[2]["text"] != "what's next?" {
		t.Fatalf("user text must remain the final block, got %q", blocks[2]["text"])
	}
}

func TestPromptBlocksHistoryWithoutChannel(t *testing.T) {
	// History without conversation context (defensive: not produced in practice)
	// still emits ahead of the user text rather than being dropped.
	blocks := zeroContextClient().promptBlocks(PromptRequest{Text: "hi", History: "earlier"})
	if len(blocks) != 2 {
		t.Fatalf("expected history plus user text, got %d", len(blocks))
	}
	if blocks[0]["text"] != "earlier" || blocks[1]["text"] != "hi" {
		t.Fatalf("unexpected ordering: %#v", blocks)
	}
}

func TestPromptBlocksFullOrdering(t *testing.T) {
	now := time.Date(2026, 6, 26, 14, 30, 0, 0, time.UTC)
	blocks := clockClient(now, "/work").promptBlocks(PromptRequest{
		Text:    "go",
		Channel: "C123",
		Thread:  "1699999999.000001",
		History: "earlier",
	})
	if len(blocks) != 4 {
		t.Fatalf("expected context, conversation-context, history, and text, got %d", len(blocks))
	}
	if !strings.Contains(blocks[0]["text"], "<context>") {
		t.Fatalf("first block should be the volatile context, got %q", blocks[0]["text"])
	}
	if !strings.Contains(blocks[1]["text"], "conversation-context") {
		t.Fatalf("second block should be the conversation context, got %q", blocks[1]["text"])
	}
	if blocks[2]["text"] != "earlier" || blocks[3]["text"] != "go" {
		t.Fatalf("unexpected tail ordering: %#v", blocks[2:])
	}
}
