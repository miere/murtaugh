package client

import (
	"strings"
	"testing"
)

// realQuipHTML mirrors the exact structure a Slack canvas download returns
// (captured live): a wrapping div, headings, `p.line` paragraphs, a checklist
// (div[data-section-style=7] > ul > li, checked items carry class="checked"), and
// a table.
const realQuipHTML = `<div class="quip-canvas-content">` +
	`<h1 id="temp:C:NLf16">My Canvas</h1>` +
	`<p id="temp:C:NLfea" class="line">This is a comment about the gateway.</p>` +
	`<p id="temp:C:NLfcf" class="line">Claude Code now supports Opus 5.</p>` +
	`<h2 id="temp:C:NLf6a">Tasks</h2>` +
	`<div data-section-style='7' class="list-numbering-restart-at" style="--indent0: 0">` +
	`<ul id='temp:C:NLf97'>` +
	`<li id='temp:C:NLfe9' value='1'><span id="temp:C:NLfe9">alpha unchecked</span><br/></li>` +
	`<li id='temp:C:NLfbf' class='checked'><span id="temp:C:NLfbf">beta checked</span><br/></li>` +
	`</ul></div>` +
	`<table><tr><td><p class="line">Col1</p></td><td><p class="line">Col2</p></td></tr>` +
	`<tr><td><p class="line">x</p></td><td><p class="line">y</p></td></tr></table>` +
	`</div>`

func TestCanvasHTMLToMarkdown_RealQuipShape(t *testing.T) {
	md := canvasHTMLToMarkdown(realQuipHTML)
	t.Logf("converted markdown:\n%s", md)

	checks := map[string]string{
		"h1 heading":       "# My Canvas",
		"h2 heading":       "## Tasks",
		"paragraph":        "Claude Code now supports Opus 5.",
		"unchecked task":   "- [ ] alpha unchecked",
		"checked task":     "- [x] beta checked",
		"table header row": "| Col1 | Col2 |",
	}
	for name, want := range checks {
		if !strings.Contains(md, want) {
			t.Errorf("%s: expected markdown to contain %q\n--- got ---\n%s", name, want, md)
		}
	}
	// Table body data is present (GFM pads cell widths, so match cell tokens).
	if !strings.Contains(md, "| x") || !strings.Contains(md, "| y") {
		t.Errorf("table body cells missing:\n%s", md)
	}
	// The quip render ids must not leak into the markdown.
	if strings.Contains(md, "temp:C:") {
		t.Errorf("quip temp ids leaked into markdown:\n%s", md)
	}
	// Checkboxes must be real task syntax, not escaped ("\[ \]").
	if strings.Contains(md, `\[`) {
		t.Errorf("task brackets were escaped:\n%s", md)
	}
}

func TestCanvasHTMLToMarkdown_FallsBackOnJunk(t *testing.T) {
	// Non-HTML-ish input still returns a string (never fails the read).
	if got := canvasHTMLToMarkdown("just some text"); got == "" {
		t.Fatal("expected non-empty fallback for plain text")
	}
}
