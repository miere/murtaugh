// Package jsontemplate renders JSON documents from text/template sources
// against caller-supplied data, with the escaping helpers that make
// interpolating untrusted values into JSON safe.
//
// It is deliberately standalone: no Slack types, no embedded-asset default, no
// knowledge of what the rendered JSON means. Callers supply the lookup roots
// and decode the bytes into whatever shape they need — a Slack attachment
// (internal/unfurl), a workflow response payload (internal/workflow), or Block
// Kit blocks (internal/slack/client.DecodeBlocks).
//
// # Why the escaping funcs exist
//
// text/template performs no escaping whatsoever. Interpolating a value straight
// into a JSON string literal is therefore unsafe: a value containing a double
// quote produces malformed JSON (which a caller's decode step catches, so it
// fails closed), but a value crafted to close the string and open new keys
// produces *valid* JSON with attacker-chosen structure, which no validity check
// will catch.
//
// Use these anywhere a value lands inside JSON:
//
//	"text": {{ json .URL }}                      // whole value, quotes included
//	"text": "PR #{{ jsonstr .Captures.number }}"  // inside a larger string
//
// Templates are parsed with Option("missingkey=error"), so a typo'd placeholder
// fails loudly rather than rendering a half-built document.
package jsontemplate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// FuncMap returns the template functions available to every rendered template.
// Exported so callers that build their own template.Template (rather than going
// through Renderer) get the same escaping helpers.
func FuncMap() template.FuncMap {
	return template.FuncMap{
		"json":    Value,
		"jsonstr": Inner,
	}
}

// Value renders v as a complete JSON value, quotes and all.
func Value(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Slack payloads are not HTML; escaping <, > and & to \u00xx would still
	// decode correctly but makes rendered templates needlessly unreadable.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// Inner renders v as JSON-escaped text with no surrounding quotes, for
// interpolation inside a larger JSON string literal.
func Inner(v any) (string, error) {
	encoded, err := Value(fmt.Sprint(v))
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimPrefix(encoded, `"`), `"`), nil
}

// Renderer resolves template references against a directory on disk first, then
// an optional fallback fs.FS (typically an embedded asset tree). That order lets
// an operator override a shipped template by dropping a file of the same name
// into their config directory.
type Renderer struct {
	dir  string
	fsys fs.FS
}

// New builds a Renderer rooted at dir, falling back to fsys for references not
// found on disk. An empty dir means the working directory; a nil fsys means
// disk-only (no embedded fallback). Callers that want an embedded default pass
// it explicitly — this package intentionally has no opinion about which.
func New(dir string, fsys fs.FS) *Renderer {
	if dir == "" {
		dir = "."
	}
	return &Renderer{dir: dir, fsys: fsys}
}

// Render reads the template at ref, executes it against data, and returns the
// rendered bytes.
//
// The result is NOT validated as JSON: every caller decodes it into a concrete
// type immediately afterwards, and that decode is the validity check. Returning
// raw bytes keeps each caller's existing error reporting intact rather than
// replacing it with a generic one from here.
func (r *Renderer) Render(ref string, data any) ([]byte, error) {
	resolved := r.resolve(ref)
	content, err := r.read(ref, resolved)
	if err != nil {
		return nil, fmt.Errorf("read template: %w", err)
	}
	return Execute(filepath.Base(resolved), content, data)
}

// Execute renders an in-memory template source against data. It is the seam
// Renderer.Render is built on, exposed for callers holding a template string
// rather than a file reference (the workflow engine's inline prompts, tests).
func Execute(name string, src []byte, data any) ([]byte, error) {
	tpl, err := template.New(name).Funcs(FuncMap()).Option("missingkey=error").Parse(string(src))
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute template: %w", err)
	}
	return buf.Bytes(), nil
}

func (r *Renderer) resolve(ref string) string {
	if filepath.IsAbs(ref) {
		return ref
	}
	return filepath.Join(r.dir, ref)
}

// read tries the resolved on-disk path first and falls back to the embedded FS.
// An absolute reference is never looked up in the FS: it names a specific file
// on this machine, so silently serving an embedded template of a similar name
// would be surprising.
func (r *Renderer) read(ref, resolved string) ([]byte, error) {
	content, err := os.ReadFile(resolved)
	if err == nil {
		return content, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	if r.fsys != nil && !filepath.IsAbs(ref) {
		return fs.ReadFile(r.fsys, filepath.ToSlash(ref))
	}
	return nil, err
}
