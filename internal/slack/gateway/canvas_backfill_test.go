package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

// richTextSection builds a rich_text block whose single section holds the given
// inline elements — the shape Slack uses for a canvas section's content.
func richTextBlock(elements ...slack.RichTextSectionElement) *slack.RichTextBlock {
	return &slack.RichTextBlock{
		Type:     slack.MBTRichText,
		Elements: []slack.RichTextElement{&slack.RichTextSection{Elements: elements}},
	}
}

func TestRichTextToPlain_RendersInlineElements(t *testing.T) {
	blocks := slack.Blocks{BlockSet: []slack.Block{richTextBlock(
		&slack.RichTextSectionTextElement{Text: "hello "},
		&slack.RichTextSectionUserElement{UserID: "U123"},
		&slack.RichTextSectionTextElement{Text: " see "},
		&slack.RichTextSectionLinkElement{URL: "https://x.test", Text: "here"},
		&slack.RichTextSectionTextElement{Text: " "},
		&slack.RichTextSectionEmojiElement{Name: "wave"},
	)}}
	got := richTextToPlain(blocks)
	want := "hello <@U123> see here :wave:"
	if got != want {
		t.Fatalf("richTextToPlain = %q, want %q", got, want)
	}
}

func TestRichTextToPlain_EmptyOnNoRichText(t *testing.T) {
	if got := richTextToPlain(slack.Blocks{}); got != "" {
		t.Fatalf("expected empty string for no blocks, got %q", got)
	}
}

// TestBackfill_RendersCanvasSectionFromBlocks is the Slice A fix: a canvas comment
// thread's root (subtype document_comment_root) carries its section text in blocks
// with an empty top-level text. It must be rendered (not dropped) and labelled as
// the tagged canvas section, so the agent stops replying "I don't see anything
// above this message".
func TestBackfill_RendersCanvasSectionFromBlocks(t *testing.T) {
	canvasRoot := slack.Message{Msg: slack.Msg{
		Timestamp: "1700000000.000100",
		User:      "USLACKBOT",
		SubType:   subtypeDocumentCommentRoot,
		Text:      "", // canvas section content lives in blocks, not text
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			richTextBlock(&slack.RichTextSectionTextElement{Text: "Claude Code now supports Opus 5"}),
		}},
	}}
	reply := msg("1700000000.000200", "U1", "is this true?")

	api := &fakeBackfillAPI{
		replies: []slack.Message{canvasRoot, reply},
		users:   map[string]*slack.User{"U1": userWithDisplayName("miere")},
	}
	b := NewThreadBackfiller(api, "UBOT", nil)

	// excludeTS points at a message not present, so both lines are kept.
	out, err := b.Backfill(context.Background(), "C1", "1700000000.000100", "1700000000.000999")
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if !strings.Contains(out, "Claude Code now supports Opus 5") {
		t.Fatalf("canvas section text (from blocks) missing — the #9.1 bug:\n%s", out)
	}
	if !strings.Contains(out, "canvas section you were tagged in") {
		t.Fatalf("canvas root should be labelled as the tagged section, got:\n%s", out)
	}
	if !strings.Contains(out, "@miere: is this true?") {
		t.Fatalf("normal reply should still render, got:\n%s", out)
	}
	// Oldest-first: the canvas section precedes the reply.
	if strings.Index(out, "Opus 5") > strings.Index(out, "is this true?") {
		t.Fatalf("canvas section should precede the reply (oldest-first):\n%s", out)
	}
}

// TestBackfill_NonCanvasBlockMessageStillRenders proves the blocks fallback is
// general: a normal message with empty text but rich_text blocks is no longer
// dropped, and is labelled as an ordinary author line (not a canvas section).
func TestBackfill_NonCanvasBlockMessageStillRenders(t *testing.T) {
	blockOnly := slack.Message{Msg: slack.Msg{
		Timestamp: "1700000000.000100",
		User:      "U1",
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			richTextBlock(&slack.RichTextSectionTextElement{Text: "posted via blocks"}),
		}},
	}}
	api := &fakeBackfillAPI{
		replies: []slack.Message{blockOnly},
		users:   map[string]*slack.User{"U1": userWithDisplayName("miere")},
	}
	b := NewThreadBackfiller(api, "UBOT", nil)

	out, err := b.Backfill(context.Background(), "C1", "1700000000.000100", "1700000000.000999")
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if !strings.Contains(out, "@miere: posted via blocks") {
		t.Fatalf("block-only message should render as a normal author line, got:\n%s", out)
	}
	if strings.Contains(out, "canvas section") {
		t.Fatalf("non-canvas message must not get the canvas label, got:\n%s", out)
	}
}
