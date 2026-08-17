package unfurl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/jsontemplate"
	"github.com/slack-go/slack"
)

// Data is the context exposed to unfurl templates and passed as JSON on stdin
// to `run` handlers. Field names are intentionally exported and stable.
type Data struct {
	URL       string
	Domain    string
	Channel   string
	User      string
	MessageTS string
	ThreadTS  string
	TeamID    string
	Captures  map[string]string
}

// Renderer turns a Block Kit JSON template into a slack.Attachment. The
// template lookup, escaping funcs, and missingkey=error discipline live in
// internal/jsontemplate; this type adds the unfurl-specific part — decoding the
// rendered bytes into a slack.Attachment.
type Renderer struct {
	tpl *jsontemplate.Renderer
}

// NewRenderer builds a Renderer. A nil templateFS falls back to assets.FS, so
// the shipped templates under assets/templates/unfurl resolve out of the box.
func NewRenderer(templateDir string, templateFS fs.FS) *Renderer {
	if templateFS == nil {
		templateFS = assets.FS
	}
	return &Renderer{tpl: jsontemplate.New(templateDir, templateFS)}
}

// Render parses and executes the template at path with data, returning the
// decoded attachment.
func (r *Renderer) Render(path string, data Data) (slack.Attachment, error) {
	rendered, err := r.tpl.Render(path, data)
	if err != nil {
		return slack.Attachment{}, err
	}
	return ParseAttachment(rendered)
}

// RenderPrompt renders a delegate-to-agent prompt template against the unfurl
// Data (so prompts can reference {{ .URL }}, {{ .Captures.number }}, etc.),
// using missingkey=error so a typo'd placeholder fails loudly rather than
// sending the agent a half-rendered prompt.
func RenderPrompt(promptTemplate string, data Data) (string, error) {
	out, err := jsontemplate.Execute("prompt", []byte(promptTemplate), data)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ParseAttachment decodes a single Slack attachment (which may carry Block Kit
// blocks) from JSON, rejecting malformed output.
func ParseAttachment(body []byte) (slack.Attachment, error) {
	trimmed := bytes.TrimSpace(body)
	if !json.Valid(trimmed) {
		return slack.Attachment{}, fmt.Errorf("unfurl output must be valid JSON")
	}
	var attachment slack.Attachment
	if err := json.Unmarshal(trimmed, &attachment); err != nil {
		return slack.Attachment{}, fmt.Errorf("decode unfurl attachment: %w", err)
	}
	return attachment, nil
}
