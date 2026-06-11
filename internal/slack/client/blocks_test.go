package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveBlocks_EmptyReturnsNil(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t "} {
		got, err := ResolveBlocks(in)
		if err != nil {
			t.Fatalf("ResolveBlocks(%q): %v", in, err)
		}
		if got != nil {
			t.Fatalf("ResolveBlocks(%q) = %q, want nil", in, got)
		}
	}
}

func TestResolveBlocks_InlineArrayPassThrough(t *testing.T) {
	in := `[{"type":"section","text":{"type":"mrkdwn","text":"hi"}}]`
	got, err := ResolveBlocks(in)
	if err != nil {
		t.Fatalf("ResolveBlocks: %v", err)
	}
	if string(got) != in {
		t.Fatalf("ResolveBlocks = %q, want %q", got, in)
	}
}

func TestResolveBlocks_InlineObjectPassThrough(t *testing.T) {
	in := `  {"type":"section"}  `
	got, err := ResolveBlocks(in)
	if err != nil {
		t.Fatalf("ResolveBlocks: %v", err)
	}
	if string(got) != `{"type":"section"}` {
		t.Fatalf("ResolveBlocks = %q", got)
	}
}

func TestResolveBlocks_InlineInvalidJSONErrors(t *testing.T) {
	_, err := ResolveBlocks("{not json")
	if err == nil || !strings.Contains(err.Error(), "Error parsing blocks JSON: invalid JSON") {
		t.Fatalf("ResolveBlocks err = %v, want invalid-JSON error", err)
	}
}

func TestResolveBlocks_FilePathReadsAndValidates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blocks.json")
	body := `[{"type":"section","text":{"type":"mrkdwn","text":"from file"}}]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := ResolveBlocks(path)
	if err != nil {
		t.Fatalf("ResolveBlocks: %v", err)
	}
	if string(got) != body {
		t.Fatalf("ResolveBlocks = %q, want %q", got, body)
	}
}

func TestResolveBlocks_FileMissingErrors(t *testing.T) {
	_, err := ResolveBlocks("/nope/does/not/exist.json")
	if err == nil || !strings.Contains(err.Error(), "Error parsing blocks JSON: cannot read file") {
		t.Fatalf("ResolveBlocks err = %v, want cannot-read-file error", err)
	}
}

func TestResolveBlocks_FileWithInvalidJSONErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("not json at all"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := ResolveBlocks(path)
	if err == nil || !strings.Contains(err.Error(), "does not contain valid JSON") {
		t.Fatalf("ResolveBlocks err = %v, want invalid-file-JSON error", err)
	}
}
