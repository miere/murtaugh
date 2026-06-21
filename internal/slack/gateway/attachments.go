package gateway

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

// fileFetcher downloads a Slack file's bytes to a writer using the bot's
// credentials (url_private requires auth). *slack.Client satisfies it via
// GetFileContext.
type fileFetcher interface {
	GetFileContext(ctx context.Context, downloadURL string, w io.Writer) error
}

const (
	// maxAttachmentFileBytes caps how much of a single text file is folded into
	// the prompt; larger text files are noted as truncated.
	maxAttachmentFileBytes = 256 * 1024
	// maxAttachmentTotalBytes caps the combined attachment text across one
	// message, so a burst of files cannot blow up the prompt (and the context).
	maxAttachmentTotalBytes = 1 << 20
	// maxAttachmentFiles caps how many files from one message are processed.
	maxAttachmentFiles = 10
	// attachmentFetchTimeout bounds a single file download.
	attachmentFetchTimeout = 20 * time.Second
)

// textMimeAllow lists non-"text/*" mimetypes that are still plain text we can
// fold into the prompt.
var textMimeAllow = map[string]bool{
	"application/json":          true,
	"application/xml":           true,
	"application/yaml":          true,
	"application/x-yaml":        true,
	"application/javascript":    true,
	"application/x-javascript":  true,
	"application/x-sh":          true,
	"application/x-shellscript": true,
	"application/toml":          true,
	"application/sql":           true,
	"application/x-ndjson":      true,
}

// textFiletypeAllow lists Slack `filetype` labels that are plain text even when
// Slack reports a non-text mimetype.
var textFiletypeAllow = map[string]bool{
	"text": true, "markdown": true, "md": true, "csv": true, "tsv": true,
	"json": true, "yaml": true, "yml": true, "xml": true, "toml": true,
	"ini": true, "log": true, "sql": true, "sh": true, "bash": true,
	"python": true, "javascript": true, "typescript": true, "go": true,
	"java": true, "c": true, "cpp": true, "rust": true, "ruby": true,
	"php": true, "html": true, "css": true, "diff": true, "patch": true,
}

// isTextFile reports whether a Slack file is plain text we can read and fold
// into the prompt, by mimetype (text/* or an allowlisted type) or filetype.
func isTextFile(f slack.File) bool {
	mt := strings.ToLower(strings.TrimSpace(f.Mimetype))
	if strings.HasPrefix(mt, "text/") || textMimeAllow[mt] {
		return true
	}
	return textFiletypeAllow[strings.ToLower(strings.TrimSpace(f.Filetype))]
}

// attachmentDownloadURL picks the URL to fetch a file from, preferring the
// download variant.
func attachmentDownloadURL(f slack.File) string {
	if f.URLPrivateDownload != "" {
		return f.URLPrivateDownload
	}
	return f.URLPrivate
}

// renderAttachments fetches the plain-text files attached to a message and
// returns a delimited block to append to the prompt so the agent can read them.
// Non-text, oversized, or unreadable files are listed by name and type rather
// than dropped silently, so the agent (and user) know something was attached.
// Returns "" when there is nothing to fold in or no fetcher is wired.
func (h *ChatHandler) renderAttachments(ctx context.Context, files []slack.File) string {
	if h.fileFetcher == nil || len(files) == 0 {
		return ""
	}

	var b strings.Builder
	total := 0
	processed := 0
	for _, f := range files {
		if processed >= maxAttachmentFiles {
			fmt.Fprintf(&b, "<file name=%q note=%q/>\n", f.Name, "and further files omitted (too many attachments)")
			break
		}
		processed++

		name := f.Name
		if name == "" {
			name = f.Title
		}
		mt := f.Mimetype
		if mt == "" {
			mt = f.Filetype
		}

		if !isTextFile(f) {
			fmt.Fprintf(&b, "<file name=%q type=%q bytes=%d note=%q/>\n", name, mt, f.Size, "binary or unsupported type; not shown")
			continue
		}
		if total >= maxAttachmentTotalBytes {
			fmt.Fprintf(&b, "<file name=%q type=%q bytes=%d note=%q/>\n", name, mt, f.Size, "omitted (attachment size budget exceeded)")
			continue
		}

		url := attachmentDownloadURL(f)
		if url == "" {
			fmt.Fprintf(&b, "<file name=%q type=%q note=%q/>\n", name, mt, "no download URL")
			continue
		}

		content, truncated, err := h.fetchTextFile(ctx, url, maxAttachmentFileBytes)
		if err != nil {
			h.logger.Warn("attachment fetch failed", "file", name, "error", err)
			fmt.Fprintf(&b, "<file name=%q type=%q note=%q/>\n", name, mt, "could not be read")
			continue
		}
		if remaining := maxAttachmentTotalBytes - total; len(content) > remaining {
			content = content[:remaining]
			truncated = true
		}
		total += len(content)

		truncAttr := ""
		if truncated {
			truncAttr = ` truncated="true"`
		}
		fmt.Fprintf(&b, "<file name=%q type=%q bytes=%d%s>\n%s\n</file>\n", name, mt, f.Size, truncAttr, content)
	}

	if b.Len() == 0 {
		return ""
	}
	return "<attachments>\nThe user attached the following file(s); their contents are included verbatim.\n\n" +
		strings.TrimRight(b.String(), "\n") + "\n</attachments>"
}

// fetchTextFile downloads up to limit bytes of a text file, reporting whether it
// was truncated.
func (h *ChatHandler) fetchTextFile(ctx context.Context, url string, limit int) (content string, truncated bool, err error) {
	fetchCtx, cancel := context.WithTimeout(ctx, attachmentFetchTimeout)
	defer cancel()

	var buf bytes.Buffer
	if err := h.fileFetcher.GetFileContext(fetchCtx, url, &buf); err != nil {
		return "", false, err
	}
	if buf.Len() > limit {
		return string(buf.Bytes()[:limit]), true, nil
	}
	return buf.String(), false, nil
}
