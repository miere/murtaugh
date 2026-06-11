package unfurl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemplate(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write template: %v", err)
	}
}

func TestRendererRendersFromDir(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "tpl.json", `{"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"PR {{ .Captures.number }} {{ .URL }}"}}]}`)
	r := NewRenderer(dir, nil)
	att, err := r.Render("tpl.json", Data{URL: "https://x/pull/7", Captures: map[string]string{"number": "7"}})
	if err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	if len(att.Blocks.BlockSet) != 1 {
		t.Fatalf("expected 1 block, got %d", len(att.Blocks.BlockSet))
	}
	out, _ := json.Marshal(att)
	if !strings.Contains(string(out), "PR 7 https://x/pull/7") {
		t.Fatalf("unexpected rendered attachment: %s", out)
	}
}

func TestRendererMissingKeyErrors(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "tpl.json", `{"blocks":[{"type":"section","text":{"type":"mrkdwn","text":"{{ .Captures.missing }}"}}]}`)
	r := NewRenderer(dir, nil)
	if _, err := r.Render("tpl.json", Data{Captures: map[string]string{}}); err == nil {
		t.Fatal("expected missingkey error")
	}
}

func TestRendererRejectsInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "tpl.json", `not json {{ .URL }}`)
	r := NewRenderer(dir, nil)
	if _, err := r.Render("tpl.json", Data{URL: "u"}); err == nil {
		t.Fatal("expected invalid JSON error")
	}
}

func TestRendererFallsBackToEmbeddedAssets(t *testing.T) {
	r := NewRenderer(t.TempDir(), nil)
	att, err := r.Render("templates/unfurl/github-pr.json", Data{
		URL:      "https://github.com/acme/widgets/pull/42",
		Captures: map[string]string{"owner": "acme", "repo": "widgets", "number": "42"},
	})
	if err != nil {
		t.Fatalf("embedded render failed: %v", err)
	}
	out, _ := json.Marshal(att)
	if !strings.Contains(string(out), "Pull Request #42") {
		t.Fatalf("unexpected embedded attachment: %s", out)
	}
}

func TestParseAttachmentRejectsInvalid(t *testing.T) {
	if _, err := ParseAttachment([]byte("nope")); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}
