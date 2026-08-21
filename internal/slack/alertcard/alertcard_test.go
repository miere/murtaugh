package alertcard

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/miere/murtaugh/assets"
	slacklib "github.com/miere/murtaugh/internal/slack/client"
)

func testRenderer() *Renderer { return NewRenderer("", assets.FS) }

// card decodes a rendered template far enough to assert on its structure.
type card struct {
	Blocks []struct {
		Type            string `json:"type"`
		BlockID         string `json:"block_id"`
		IsCollapsible   bool   `json:"is_collapsible"`
		DefaultCollapse bool   `json:"default_collapsed"`
		Width           string `json:"width"`
		Title           struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"title"`
		Subtitle *struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"subtitle"`
		Icon struct {
			ImageURL string `json:"image_url"`
			AltText  string `json:"alt_text"`
		} `json:"icon"`
		ChildBlocks []struct {
			Type string `json:"type"`
			Text struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"text"`
			Elements json.RawMessage `json:"elements"`
		} `json:"child_blocks"`
	} `json:"blocks"`
}

// render renders a spec and runs every universal assertion over the result.
func render(t *testing.T, spec Spec) (card, []byte) {
	t.Helper()
	raw, err := testRenderer().Render(spec)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var c card
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("rendered template is not valid JSON: %v\n%s", err, raw)
	}
	if len(c.Blocks) != 1 {
		t.Fatalf("want exactly one block, got %d\n%s", len(c.Blocks), raw)
	}
	b := c.Blocks[0]
	if b.Type != "container" {
		t.Errorf("block type = %q, want container", b.Type)
	}
	if b.BlockID != ContainerBlockID {
		t.Errorf("block_id = %q, want %q", b.BlockID, ContainerBlockID)
	}
	// The whole design rests on the card being folded on arrival.
	if !b.IsCollapsible || !b.DefaultCollapse {
		t.Errorf("is_collapsible=%v default_collapsed=%v, want both true", b.IsCollapsible, b.DefaultCollapse)
	}
	// A collapsed container with nothing inside is a box that opens onto
	// nothing; every spec must produce at least one child.
	if len(b.ChildBlocks) == 0 {
		t.Errorf("child_blocks is empty\n%s", raw)
	}
	// The client must pass these bytes through untouched — that is the whole
	// reason this is a template and not a slack-go builder.
	if _, err := slacklib.DecodeBlocks(raw); err != nil {
		t.Errorf("DecodeBlocks rejected the card: %v", err)
	}
	return c, raw
}

func TestRenderLevelPicksIconAndTitle(t *testing.T) {
	tests := []struct {
		name      string
		level     Level
		wantIcon  string
		wantTitle string
		wantAlt   string
	}{
		{"error", LevelError, iconError, "Oops! Something went wrong!", "Error icon"},
		{"warn", LevelWarn, iconWarn, "Heads up", "Warning icon"},
		{"info", LevelInfo, iconInfo, "Good to know", "Information icon"},
		// An unset level is a caller slip. Shouting is recoverable; silently
		// downgrading a real failure to an info note is not.
		{"unset falls back to error", Level(""), iconError, "Oops! Something went wrong!", "Error icon"},
		{"unknown falls back to error", Level("catastrophe"), iconError, "Oops! Something went wrong!", "Error icon"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := render(t, Spec{Level: tc.level, Subtitle: "something happened"})
			b := c.Blocks[0]
			if b.Icon.ImageURL != tc.wantIcon {
				t.Errorf("icon = %q, want %q", b.Icon.ImageURL, tc.wantIcon)
			}
			if b.Icon.AltText != tc.wantAlt {
				t.Errorf("alt_text = %q, want %q", b.Icon.AltText, tc.wantAlt)
			}
			if b.Title.Text != tc.wantTitle {
				t.Errorf("title = %q, want %q", b.Title.Text, tc.wantTitle)
			}
			if b.Title.Type != "plain_text" {
				t.Errorf("title type = %q, want plain_text", b.Title.Type)
			}
		})
	}
}

func TestRenderTitleOverride(t *testing.T) {
	c, _ := render(t, Spec{Level: LevelError, Title: "Claude Code died unexpectedly", Subtitle: "x"})
	if got := c.Blocks[0].Title.Text; got != "Claude Code died unexpectedly" {
		t.Errorf("title = %q, want the override", got)
	}
}

// An absent subtitle must drop the key rather than emit an empty plain_text
// object, which Slack rejects.
func TestRenderOmitsEmptySubtitle(t *testing.T) {
	c, _ := render(t, Spec{Level: LevelInfo, Text: "body"})
	if c.Blocks[0].Subtitle != nil {
		t.Errorf("subtitle rendered when empty: %+v", c.Blocks[0].Subtitle)
	}

	c, _ = render(t, Spec{Level: LevelInfo, Subtitle: "a summary", Text: "body"})
	if c.Blocks[0].Subtitle == nil || c.Blocks[0].Subtitle.Text != "a summary" {
		t.Errorf("subtitle = %+v, want %q", c.Blocks[0].Subtitle, "a summary")
	}
}

func TestRenderBodyOrderAndLabels(t *testing.T) {
	c, _ := render(t, Spec{
		Level:     LevelWarn,
		Subtitle:  "The agent ended the turn without a reply.",
		Reason:    "`tool_use`",
		Text:      "The turn stopped before any text was produced.",
		NextSteps: "Ask the agent to try again.",
	})

	body := c.Blocks[0].ChildBlocks[0]
	if body.Type != "section" || body.Text.Type != "mrkdwn" {
		t.Fatalf("body block = %q/%q, want section/mrkdwn", body.Type, body.Text.Type)
	}
	want := "*Reason*: `tool_use`\n\n" +
		"The turn stopped before any text was produced.\n\n" +
		"*Next Steps*: Ask the agent to try again."
	if body.Text.Text != want {
		t.Errorf("body =\n%q\nwant\n%q", body.Text.Text, want)
	}
}

// Each body part is optional, and a missing one must not leave its label or a
// stray blank line behind.
func TestRenderPartialBodies(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
		want string
	}{
		{"reason only", Spec{Level: LevelError, Reason: "503"}, "*Reason*: 503"},
		{"text only", Spec{Level: LevelError, Text: "just prose"}, "just prose"},
		{"next steps only", Spec{Level: LevelError, NextSteps: "retry"}, "*Next Steps*: retry"},
		{"reason and next steps", Spec{Level: LevelError, Reason: "503", NextSteps: "retry"}, "*Reason*: 503\n\n*Next Steps*: retry"},
		{"reason and text", Spec{Level: LevelError, Reason: "503", Text: "prose"}, "*Reason*: 503\n\nprose"},
		{"text and next steps", Spec{Level: LevelError, Text: "prose", NextSteps: "retry"}, "prose\n\n*Next Steps*: retry"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := render(t, tc.spec)
			if got := c.Blocks[0].ChildBlocks[0].Text.Text; got != tc.want {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderDetailIsPreformatted(t *testing.T) {
	detail := `native: provider stream: llm: gemini stream: HTTP 503: {"error": {"code": 503}}`
	c, raw := render(t, Spec{Level: LevelError, Subtitle: "agent unavailable", Reason: "overloaded", Detail: detail})

	kids := c.Blocks[0].ChildBlocks
	if len(kids) != 2 {
		t.Fatalf("want body + detail child blocks, got %d\n%s", len(kids), raw)
	}
	if kids[1].Type != "rich_text" {
		t.Errorf("detail block type = %q, want rich_text", kids[1].Type)
	}
	if !strings.Contains(string(raw), "rich_text_preformatted") {
		t.Errorf("detail is not preformatted\n%s", raw)
	}
	// The raw diagnostic must survive intact — collapsing is what makes it
	// affordable to keep in full.
	var elems []struct {
		Type     string `json:"type"`
		Elements []struct {
			Text string `json:"text"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(kids[1].Elements, &elems); err != nil {
		t.Fatalf("decode rich_text elements: %v", err)
	}
	if got := elems[0].Elements[0].Text; got != detail {
		t.Errorf("detail = %q, want %q", got, detail)
	}
}

// Detail alone is a valid alert: the body section disappears and the
// preformatted block stands on its own.
func TestRenderDetailWithoutBody(t *testing.T) {
	c, _ := render(t, Spec{Level: LevelError, Subtitle: "boom", Detail: "stack trace"})
	kids := c.Blocks[0].ChildBlocks
	if len(kids) != 1 || kids[0].Type != "rich_text" {
		t.Fatalf("want only the rich_text child, got %d blocks (first %q)", len(kids), kids[0].Type)
	}
}

func TestRenderEmptyBodyGetsLevelGuidance(t *testing.T) {
	tests := []struct {
		level Level
		want  string
	}{
		{LevelError, "Try again. If it keeps happening, notify your admin user."},
		{LevelWarn, "Nothing to do right now — worth a look if it keeps happening."},
		{LevelInfo, "No action needed."},
	}
	for _, tc := range tests {
		t.Run(string(tc.level), func(t *testing.T) {
			c, _ := render(t, Spec{Level: tc.level, Subtitle: "a summary"})
			want := "*Next Steps*: " + tc.want
			if got := c.Blocks[0].ChildBlocks[0].Text.Text; got != want {
				t.Errorf("body = %q, want %q", got, want)
			}
		})
	}
}

// A caller who supplied any body at all must not have guidance invented for
// them on top of it.
func TestRenderDoesNotInventGuidanceOverAGivenBody(t *testing.T) {
	c, _ := render(t, Spec{Level: LevelError, Text: "the only thing worth saying"})
	if got := c.Blocks[0].ChildBlocks[0].Text.Text; got != "the only thing worth saying" {
		t.Errorf("body = %q, want the caller's text alone", got)
	}
}

func TestRenderClampsEveryField(t *testing.T) {
	long := strings.Repeat("x", 20_000)
	c, raw := render(t, Spec{
		Level:     LevelError,
		Title:     long,
		Subtitle:  long,
		Reason:    long,
		Text:      long,
		NextSteps: long,
		Detail:    long,
	})

	b := c.Blocks[0]
	if len([]rune(b.Title.Text)) != titleLimit {
		t.Errorf("title len = %d, want %d", len([]rune(b.Title.Text)), titleLimit)
	}
	if len([]rune(b.Subtitle.Text)) != subtitleLimit {
		t.Errorf("subtitle len = %d, want %d", len([]rune(b.Subtitle.Text)), subtitleLimit)
	}
	// The body is three clamped parts plus the labels; the point is that the
	// total lands under Slack's 3000-rune section limit.
	if n := len([]rune(b.ChildBlocks[0].Text.Text)); n > 3000 {
		t.Errorf("body len = %d, want <= 3000", n)
	}
	if !strings.Contains(string(raw), "…") {
		t.Errorf("truncation is not visible in the output")
	}
}

func TestClampCountsRunesNotBytes(t *testing.T) {
	// Cutting on a byte boundary would corrupt the last character.
	got := clamp(strings.Repeat("é", 100), 10)
	if len([]rune(got)) != 10 {
		t.Errorf("clamped to %d runes, want 10", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("clamp did not mark the truncation: %q", got)
	}
	if got := clamp("short", 10); got != "short" {
		t.Errorf("clamp altered a string under the limit: %q", got)
	}
}

// Error text is full of quotes and braces, which makes this the highest-risk
// interpolation surface in the codebase: a crafted value must not be able to
// close its JSON string and append sibling blocks.
func TestRenderEscapesHostileText(t *testing.T) {
	hostile := `", "type": "divider"}, {"type": "section", "text": {"type": "mrkdwn", "text": "pwned`
	c, raw := render(t, Spec{
		Level:    LevelError,
		Title:    hostile,
		Subtitle: hostile,
		Reason:   hostile,
		Text:     hostile,
		Detail:   hostile,
	})

	if len(c.Blocks) != 1 {
		t.Fatalf("injection created %d blocks\n%s", len(c.Blocks), raw)
	}
	if n := len(c.Blocks[0].ChildBlocks); n != 2 {
		t.Fatalf("injection created %d child blocks, want 2\n%s", n, raw)
	}
	if strings.Contains(c.Blocks[0].ChildBlocks[0].Text.Text, `"type": "divider"`) {
		// It should be present as literal text, not as structure — which the
		// block counts above already prove. This only guards the value itself
		// surviving verbatim.
		if !strings.Contains(c.Blocks[0].ChildBlocks[0].Text.Text, hostile) {
			t.Errorf("hostile text was mangled rather than escaped")
		}
	}
}

// Newlines and tabs in a diagnostic must survive as JSON escapes rather than
// breaking the document.
func TestRenderHandlesControlCharacters(t *testing.T) {
	c, _ := render(t, Spec{Level: LevelError, Subtitle: "x", Detail: "line one\n\tline two"})
	var elems []struct {
		Elements []struct {
			Text string `json:"text"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(c.Blocks[0].ChildBlocks[0].Elements, &elems); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := elems[0].Elements[0].Text; got != "line one\n\tline two" {
		t.Errorf("detail = %q, want the control characters preserved", got)
	}
}

func TestFallbackText(t *testing.T) {
	tests := []struct {
		name string
		spec Spec
		want string
	}{
		{
			"title and subtitle",
			Spec{Level: LevelError, Title: "Oops!", Subtitle: "The agent is unavailable."},
			"Oops! — The agent is unavailable.",
		},
		{
			"title only",
			Spec{Level: LevelWarn, Title: "Heads up"},
			"Heads up",
		},
		{
			"defaults apply",
			Spec{Level: LevelInfo, Subtitle: "Restart is not available here."},
			"Good to know — Restart is not available here.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := FallbackText(tc.spec); got != tc.want {
				t.Errorf("FallbackText = %q, want %q", got, tc.want)
			}
		})
	}
}

// A push notification is the least private surface Slack has, so the diagnostic
// must not ride along in it.
func TestFallbackTextOmitsDetail(t *testing.T) {
	got := FallbackText(Spec{
		Level:    LevelError,
		Title:    "Oops!",
		Subtitle: "boom",
		Detail:   "AKIAIOSFODNN7EXAMPLE",
		Reason:   "a secret-looking reason",
	})
	if strings.Contains(got, "AKIA") || strings.Contains(got, "secret-looking") {
		t.Errorf("FallbackText leaked the body: %q", got)
	}
}

func TestPlainTextMirrorsTheCard(t *testing.T) {
	got := PlainText(Spec{
		Level:     LevelError,
		Title:     "Oops! Something went wrong!",
		Subtitle:  "The agent is not available.",
		Reason:    "Gemini is overloaded (503)",
		NextSteps: "Try again shortly.",
		Detail:    "native: provider stream: HTTP 503",
	})
	want := ":x: *Oops! Something went wrong!*\n" +
		"The agent is not available.\n\n" +
		"*Reason*: Gemini is overloaded (503)\n\n" +
		"*Next Steps*: Try again shortly.\n" +
		"```\nnative: provider stream: HTTP 503\n```"
	if got != want {
		t.Errorf("PlainText =\n%q\nwant\n%q", got, want)
	}
}

func TestPlainTextLevelEmoji(t *testing.T) {
	tests := []struct {
		level Level
		want  string
	}{
		{LevelError, ":x:"},
		{LevelWarn, ":warning:"},
		{LevelInfo, ":information_source:"},
		{Level(""), ":x:"},
	}
	for _, tc := range tests {
		t.Run(string(tc.level), func(t *testing.T) {
			if got := PlainText(Spec{Level: tc.level, Title: "t"}); !strings.HasPrefix(got, tc.want) {
				t.Errorf("PlainText = %q, want prefix %q", got, tc.want)
			}
		})
	}
}

func TestPlainTextClamps(t *testing.T) {
	long := strings.Repeat("x", 20_000)
	got := PlainText(Spec{Level: LevelError, Title: long, Subtitle: long, Reason: long, Text: long, NextSteps: long, Detail: long})
	// Every part clamped, so the whole stays well inside Slack's message limit.
	if len([]rune(got)) > 7000 {
		t.Errorf("PlainText len = %d, want a clamped result", len([]rune(got)))
	}
}
