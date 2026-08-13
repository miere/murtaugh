package unfurl

import (
	"encoding/json"
	"strings"
	"testing"
)

// A value that closes the enclosing string, block and object, then appends
// blocks of its own. Interpolated bare into a "text" field it yields *valid*
// JSON carrying attacker-chosen structure — so the ParseAttachment validity
// check does not catch it. Only escaping does.
const blockInjection = `x"}},{"type":"divider"},{"type":"section","text":{"type":"mrkdwn","text":"injected`

func TestJsonStrEscapesQuotesInsideStringLiteral(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "tpl.json",
		`{"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"repo {{ jsonstr .Captures.repo }}"}}]}`)

	r := NewRenderer(dir, nil)
	att, err := r.Render("tpl.json", Data{Captures: map[string]string{"repo": blockInjection}})
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	if len(att.Blocks.BlockSet) != 1 {
		t.Fatalf("expected 1 block, got %d — structure was injected", len(att.Blocks.BlockSet))
	}
	out, _ := json.Marshal(att)
	if strings.Contains(string(out), `"text":"injected"`) {
		t.Fatalf("injected block survived: %s", out)
	}
}

// Documents the hazard the funcs exist to avoid: the same template without
// jsonstr admits the injection. If this ever stops holding, text/template has
// changed behaviour and the funcs may be redundant.
func TestBareInterpolationIsUnsafe(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "tpl.json",
		`{"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"repo {{ .Captures.repo }}"}}]}`)

	r := NewRenderer(dir, nil)
	att, err := r.Render("tpl.json", Data{Captures: map[string]string{"repo": blockInjection}})
	if err != nil {
		t.Skipf("bare interpolation already rejected (%v) — hazard no longer reproduces", err)
	}
	if len(att.Blocks.BlockSet) == 1 {
		t.Fatal("expected the unescaped template to admit an extra block; it did not")
	}
}

func TestJsonRendersCompleteValueWithQuotes(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "tpl.json",
		`{"blocks":[{"type":"section","text":{"type":"mrkdwn","text":{{ json .URL }}}}]}`)

	const url = `https://x/pull/7?a=1&b="2"`
	r := NewRenderer(dir, nil)
	att, err := r.Render("tpl.json", Data{URL: url})
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}

	// Compare the decoded value, not the encoded bytes: slack-go's own
	// marshalling escapes & as &, which decodes back to the same string.
	out, _ := json.Marshal(att.Blocks.BlockSet[0])
	var block struct {
		Text struct {
			Text string `json:"text"`
		} `json:"text"`
	}
	if err := json.Unmarshal(out, &block); err != nil {
		t.Fatalf("decode block: %v", err)
	}
	if block.Text.Text != url {
		t.Fatalf("text = %q, want %q", block.Text.Text, url)
	}
}

// Slack payloads are not HTML; &, < and > should stay literal rather than
// becoming & and friends.
func TestJsonValueDoesNotHTMLEscape(t *testing.T) {
	got, err := jsonValue(`a&b<c>d`)
	if err != nil {
		t.Fatalf("jsonValue: %v", err)
	}
	if want := `"a&b<c>d"`; got != want {
		t.Fatalf("jsonValue = %s, want %s", got, want)
	}
}

func TestJsonInnerStripsSurroundingQuotes(t *testing.T) {
	got, err := jsonInner(`say "hi"`)
	if err != nil {
		t.Fatalf("jsonInner: %v", err)
	}
	if want := `say \"hi\"`; got != want {
		t.Fatalf("jsonInner = %s, want %s", got, want)
	}
}

// The shipped template must actually use the escaping funcs, or the fix is
// inert for the one unfurl Murtaugh ships.
func TestShippedGithubPRTemplateEscapes(t *testing.T) {
	r := NewRenderer("", nil)
	att, err := r.Render("templates/unfurl/github-pr.json", Data{
		URL:      "https://github.com/o/r/pull/7",
		Captures: map[string]string{"number": "7", "owner": blockInjection, "repo": "r"},
	})
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	if len(att.Blocks.BlockSet) != 1 {
		t.Fatalf("expected 1 block, got %d — shipped template admits injection", len(att.Blocks.BlockSet))
	}
}
