package jsontemplate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

// A value that closes the enclosing string, block and object, then appends
// blocks of its own. Interpolated bare into a "text" field it yields *valid*
// JSON carrying attacker-chosen structure — so a decode-side validity check
// does not catch it. Only escaping does.
const blockInjection = `x"}},{"type":"divider"},{"type":"section","text":{"type":"mrkdwn","text":"injected`

func writeTemplate(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write template: %v", err)
	}
}

// blockCount decodes a {"blocks":[…]} document and reports how many blocks it
// carries — the observable an injection would inflate.
func blockCount(t *testing.T, raw []byte) int {
	t.Helper()
	var doc struct {
		Blocks []json.RawMessage `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode rendered JSON: %v (%s)", err, raw)
	}
	return len(doc.Blocks)
}

// Slack payloads are not HTML; &, < and > should stay literal rather than
// becoming &amp; and friends.
func TestValueDoesNotHTMLEscape(t *testing.T) {
	got, err := Value(`a&b<c>d`)
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if want := `"a&b<c>d"`; got != want {
		t.Fatalf("Value = %s, want %s", got, want)
	}
}

func TestInnerStripsSurroundingQuotes(t *testing.T) {
	got, err := Inner(`say "hi"`)
	if err != nil {
		t.Fatalf("Inner: %v", err)
	}
	if want := `say \"hi\"`; got != want {
		t.Fatalf("Inner = %s, want %s", got, want)
	}
}

func TestInnerEscapesInsideStringLiteral(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "tpl.json",
		`{"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"repo {{ jsonstr .Repo }}"}}]}`)

	out, err := New(dir, nil).Render("tpl.json", map[string]string{"Repo": blockInjection})
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	if n := blockCount(t, out); n != 1 {
		t.Fatalf("expected 1 block, got %d — structure was injected: %s", n, out)
	}
}

func TestValueRendersCompleteValueWithQuotes(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "tpl.json",
		`{"blocks":[{"type":"section","text":{"type":"mrkdwn","text":{{ json .URL }}}}]}`)

	const url = `https://x/pull/7?a=1&b="2"`
	out, err := New(dir, nil).Render("tpl.json", map[string]string{"URL": url})
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	var doc struct {
		Blocks []struct {
			Text struct {
				Text string `json:"text"`
			} `json:"text"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode: %v (%s)", err, out)
	}
	if len(doc.Blocks) != 1 || doc.Blocks[0].Text.Text != url {
		t.Fatalf("round-trip failed: %s", out)
	}
}

// Documents the hazard the funcs exist to avoid: the same template without
// jsonstr admits the injection.
func TestBareInterpolationIsUnsafe(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "tpl.json",
		`{"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"repo {{ .Repo }}"}}]}`)

	out, err := New(dir, nil).Render("tpl.json", map[string]string{"Repo": blockInjection})
	if err != nil {
		t.Skipf("bare interpolation already rejected (%v) — hazard no longer reproduces", err)
	}
	if n := blockCount(t, out); n == 1 {
		t.Fatal("expected the unescaped template to admit an extra block; it did not")
	}
}

func TestMissingKeyIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "tpl.json", `{"text":"{{ .Nope }}"}`)
	if _, err := New(dir, nil).Render("tpl.json", struct{}{}); err == nil {
		t.Fatal("expected a missingkey error")
	}
}

func TestRenderPrefersDiskOverEmbedded(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "tpl.json", `{"from":"disk"}`)
	fsys := fstest.MapFS{"tpl.json": &fstest.MapFile{Data: []byte(`{"from":"embedded"}`)}}

	out, err := New(dir, fsys).Render("tpl.json", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if string(out) != `{"from":"disk"}` {
		t.Fatalf("expected the on-disk template to win, got %s", out)
	}
}

func TestRenderFallsBackToEmbedded(t *testing.T) {
	fsys := fstest.MapFS{"sub/tpl.json": &fstest.MapFile{Data: []byte(`{"from":"embedded"}`)}}

	out, err := New(t.TempDir(), fsys).Render("sub/tpl.json", nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if string(out) != `{"from":"embedded"}` {
		t.Fatalf("expected the embedded template, got %s", out)
	}
}

// A nil fallback FS means disk-only: a missing reference is an error rather
// than a silent miss.
func TestRenderWithoutFallbackReportsMissing(t *testing.T) {
	if _, err := New(t.TempDir(), nil).Render("absent.json", nil); err == nil {
		t.Fatal("expected an error for a missing template")
	}
}

// An absolute reference names a specific file on this machine; it must never be
// served out of the embedded tree just because a similarly-named entry exists.
func TestAbsoluteRefNeverFallsBackToFS(t *testing.T) {
	fsys := fstest.MapFS{"tpl.json": &fstest.MapFile{Data: []byte(`{"from":"embedded"}`)}}
	abs := filepath.Join(t.TempDir(), "tpl.json") // absolute, and absent on disk

	if _, err := New("", fsys).Render(abs, nil); err == nil {
		t.Fatal("expected an absolute missing path to error, not fall back to the FS")
	}
}

func TestExecuteRendersInMemorySource(t *testing.T) {
	out, err := Execute("inline", []byte(`hello {{ jsonstr .Name }}`), map[string]string{"Name": `"x"`})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if want := `hello \"x\"`; string(out) != want {
		t.Fatalf("Execute = %s, want %s", out, want)
	}
}

func TestExecuteReportsParseErrors(t *testing.T) {
	if _, err := Execute("inline", []byte(`{{ .Unclosed `), nil); err == nil {
		t.Fatal("expected a parse error")
	}
}
