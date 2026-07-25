package gateway

import (
	"strings"

	"github.com/slack-go/slack"
)

// richTextToPlain renders a message's rich_text blocks to plain text. Slack
// delivers canvas section content (and some block-only messages) with an empty
// top-level `text` and the words carried in rich_text blocks; the backfiller uses
// this to recover that content rather than dropping the message — the bug behind
// "I don't see anything above this message" on a canvas mention (spec 021 §9.1).
//
// It walks the common rich_text shapes — sections, lists, quotes, preformatted —
// concatenating the text-bearing inline elements. Unknown or non-text elements are
// skipped, so extraction degrades to "" rather than failing.
func richTextToPlain(blocks slack.Blocks) string {
	var lines []string
	for _, blk := range blocks.BlockSet {
		rtb, ok := blk.(*slack.RichTextBlock)
		if !ok {
			continue
		}
		for _, el := range rtb.Elements {
			lines = append(lines, richTextElementLines(el)...)
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// richTextElementLines renders one top-level rich_text element to zero or more
// lines (a list yields one line per item).
func richTextElementLines(el slack.RichTextElement) []string {
	switch e := el.(type) {
	case *slack.RichTextSection:
		if s := sectionElementsToText(e.Elements); s != "" {
			return []string{s}
		}
	case *slack.RichTextQuote:
		if s := sectionElementsToText(e.Elements); s != "" {
			return []string{"> " + s}
		}
	case *slack.RichTextPreformatted:
		if s := sectionElementsToText(e.Elements); s != "" {
			return []string{s}
		}
	case *slack.RichTextList:
		var out []string
		for _, item := range e.Elements {
			for _, line := range richTextElementLines(item) {
				out = append(out, "- "+line)
			}
		}
		return out
	}
	return nil
}

// sectionElementsToText concatenates the inline elements of a rich_text section
// into a single string, rendering mentions / channels / emoji / links / broadcasts
// in a readable, plain form.
func sectionElementsToText(elements []slack.RichTextSectionElement) string {
	var sb strings.Builder
	for _, el := range elements {
		switch e := el.(type) {
		case *slack.RichTextSectionTextElement:
			sb.WriteString(e.Text)
		case *slack.RichTextSectionLinkElement:
			if e.Text != "" {
				sb.WriteString(e.Text)
			} else {
				sb.WriteString(e.URL)
			}
		case *slack.RichTextSectionUserElement:
			sb.WriteString("<@" + e.UserID + ">")
		case *slack.RichTextSectionChannelElement:
			sb.WriteString("<#" + e.ChannelID + ">")
		case *slack.RichTextSectionEmojiElement:
			sb.WriteString(":" + e.Name + ":")
		case *slack.RichTextSectionBroadcastElement:
			sb.WriteString("@" + e.Range)
		}
	}
	return sb.String()
}
