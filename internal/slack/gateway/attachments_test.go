package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

// fakeFetcher serves canned file contents by URL and records what it was asked
// to download.
type fakeFetcher struct {
	contents map[string]string
	err      error
	calls    []string
}

func (f *fakeFetcher) GetFileContext(_ context.Context, url string, w io.Writer) error {
	f.calls = append(f.calls, url)
	if f.err != nil {
		return f.err
	}
	body, ok := f.contents[url]
	if !ok {
		return errors.New("not found")
	}
	_, err := io.WriteString(w, body)
	return err
}

func newAttachmentHandler(f fileFetcher) *ChatHandler {
	return &ChatHandler{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), fileFetcher: f}
}

func TestIsTextFile(t *testing.T) {
	cases := []struct {
		f    slack.File
		want bool
	}{
		{slack.File{Mimetype: "text/plain"}, true},
		{slack.File{Mimetype: "text/markdown"}, true},
		{slack.File{Mimetype: "application/json"}, true},
		{slack.File{Mimetype: "application/octet-stream", Filetype: "yaml"}, true},
		{slack.File{Mimetype: "image/png", Filetype: "png"}, false},
		{slack.File{Mimetype: "application/pdf", Filetype: "pdf"}, false},
	}
	for _, c := range cases {
		if got := isTextFile(c.f); got != c.want {
			t.Errorf("isTextFile(%+v) = %v, want %v", c.f, got, c.want)
		}
	}
}

func TestRenderAttachmentsFoldsTextFile(t *testing.T) {
	f := &fakeFetcher{contents: map[string]string{"https://files/notes": "line one\nline two"}}
	h := newAttachmentHandler(f)

	out := h.renderAttachments(context.Background(), []slack.File{
		{Name: "notes.txt", Mimetype: "text/plain", Size: 17, URLPrivate: "https://files/notes"},
	})

	if !strings.Contains(out, "<attachments>") || !strings.Contains(out, "</attachments>") {
		t.Fatalf("expected an attachments block, got: %q", out)
	}
	if !strings.Contains(out, `name="notes.txt"`) || !strings.Contains(out, "line one\nline two") {
		t.Fatalf("expected file name and content folded in, got: %q", out)
	}
}

func TestRenderAttachmentsPrefersDownloadURL(t *testing.T) {
	f := &fakeFetcher{contents: map[string]string{"https://files/dl": "x"}}
	h := newAttachmentHandler(f)
	h.renderAttachments(context.Background(), []slack.File{
		{Name: "a.txt", Mimetype: "text/plain", URLPrivate: "https://files/priv", URLPrivateDownload: "https://files/dl"},
	})
	if len(f.calls) != 1 || f.calls[0] != "https://files/dl" {
		t.Fatalf("expected download URL to be used, got calls=%v", f.calls)
	}
}

func TestRenderAttachmentsNotesBinaryWithoutFetching(t *testing.T) {
	f := &fakeFetcher{contents: map[string]string{}}
	h := newAttachmentHandler(f)
	out := h.renderAttachments(context.Background(), []slack.File{
		{Name: "pic.png", Mimetype: "image/png", Size: 2048, URLPrivate: "https://files/pic"},
	})
	if len(f.calls) != 0 {
		t.Fatalf("binary file must not be fetched, got calls=%v", f.calls)
	}
	if !strings.Contains(out, "binary or unsupported") || !strings.Contains(out, `name="pic.png"`) {
		t.Fatalf("expected a binary note, got: %q", out)
	}
}

func TestRenderAttachmentsTruncatesOversizedFile(t *testing.T) {
	big := strings.Repeat("a", maxAttachmentFileBytes+100)
	f := &fakeFetcher{contents: map[string]string{"https://files/big": big}}
	h := newAttachmentHandler(f)
	out := h.renderAttachments(context.Background(), []slack.File{
		{Name: "big.txt", Mimetype: "text/plain", Size: len(big), URLPrivate: "https://files/big"},
	})
	if !strings.Contains(out, `truncated="true"`) {
		t.Fatalf("expected truncation marker, got len(out)=%d", len(out))
	}
}

func TestRenderAttachmentsFetchErrorIsNoted(t *testing.T) {
	f := &fakeFetcher{err: errors.New("403")}
	h := newAttachmentHandler(f)
	out := h.renderAttachments(context.Background(), []slack.File{
		{Name: "a.txt", Mimetype: "text/plain", URLPrivate: "https://files/a"},
	})
	if !strings.Contains(out, "could not be read") {
		t.Fatalf("expected a read-failure note, got: %q", out)
	}
}

func TestRenderAttachmentsNoFetcherOrNoFiles(t *testing.T) {
	if got := (&ChatHandler{}).renderAttachments(context.Background(), []slack.File{{Name: "a"}}); got != "" {
		t.Fatalf("nil fetcher must yield no block, got: %q", got)
	}
	h := newAttachmentHandler(&fakeFetcher{})
	if got := h.renderAttachments(context.Background(), nil); got != "" {
		t.Fatalf("no files must yield no block, got: %q", got)
	}
}
