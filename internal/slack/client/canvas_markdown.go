package client

import (
	"strings"

	htmlmd "github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/strikethrough"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
	"golang.org/x/net/html"
)

// canvasMarkdownConverter converts a Slack canvas's downloaded HTML (quip format)
// into Markdown, so reads yield the same syntax writes accept. Built once and
// reused — the converter is safe for concurrent use. Plugins: commonmark (core),
// table (GFM tables), strikethrough (~~del~~).
var canvasMarkdownConverter = htmlmd.NewConverter(
	htmlmd.WithPlugins(
		base.NewBasePlugin(),
		commonmark.NewCommonmarkPlugin(),
		table.NewTablePlugin(),
		strikethrough.NewStrikethroughPlugin(),
	),
)

// canvasHTMLToMarkdown parses a canvas's quip HTML, normalises its checklists and
// tables, converts to Markdown, then unescapes the task-list markers. On any
// parse/convert failure it falls back to the raw HTML so a read never fails.
func canvasHTMLToMarkdown(rawHTML string) string {
	doc, err := html.Parse(strings.NewReader(rawHTML))
	if err != nil {
		return rawHTML
	}
	normalizeCanvasDOM(doc, false)
	md, err := canvasMarkdownConverter.ConvertNode(doc)
	if err != nil {
		return rawHTML
	}
	return strings.TrimSpace(unescapeTaskMarkers(string(md)))
}

// normalizeCanvasDOM rewrites two quip-specific shapes in place before conversion:
//
//   - Checklists: quip renders one as `<div data-section-style="7">` wrapping a
//     `<ul>`, a checked item carrying `class="checked"` on its `<li>` (verified
//     against a live canvas). We prepend a "[x] "/"[ ] " marker to each item so
//     commonmark emits "- [x] …" / "- [ ] …" — v2 has no task-list support, and a
//     plain `<ul>` drops the checkbox state.
//   - Tables: quip tables are header-less (every cell is `<td>`), which makes the
//     GFM table plugin synthesize an empty header row. We promote the first row's
//     `<td>` cells to `<th>` so the first row becomes the Markdown header.
func normalizeCanvasDOM(n *html.Node, inChecklist bool) {
	checklist := inChecklist
	if n.Type == html.ElementNode {
		switch n.Data {
		case "div":
			if attrValue(n, "data-section-style") == "7" {
				checklist = true
			}
		case "table":
			promoteTableHeader(n)
		case "li":
			if checklist {
				marker := "[ ] "
				if strings.Contains(attrValue(n, "class"), "checked") {
					marker = "[x] "
				}
				n.InsertBefore(&html.Node{Type: html.TextNode, Data: marker}, n.FirstChild)
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		normalizeCanvasDOM(c, checklist)
	}
}

// promoteTableHeader turns the first row of a header-less table into a header row
// (td → th) so the GFM table plugin uses it as the Markdown header instead of
// prepending an empty one.
func promoteTableHeader(tableNode *html.Node) {
	tr := firstDescendant(tableNode, "tr")
	if tr == nil {
		return
	}
	for c := tr.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == "th" {
			return // already has a header row
		}
	}
	for c := tr.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == "td" {
			c.Data = "th"
		}
	}
}

// firstDescendant returns the first element node with the given tag, depth-first.
func firstDescendant(n *html.Node, tag string) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == tag {
			return c
		}
		if found := firstDescendant(c, tag); found != nil {
			return found
		}
	}
	return nil
}

// unescapeTaskMarkers undoes the converter's smart-escaping of the task markers we
// injected: it escapes the leading "[" (`\[ ] ` / `\[x] `) because it could look
// like a link. We injected those markers ourselves, so restoring them to real
// task-list syntax is safe and targeted.
func unescapeTaskMarkers(md string) string {
	md = strings.ReplaceAll(md, `\[ ] `, `[ ] `)
	md = strings.ReplaceAll(md, `\[x] `, `[x] `)
	return md
}

func attrValue(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
