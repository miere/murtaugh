// Package update implements the `setup.update` tool: report whether a newer
// Murtaugh release exists and point at its notes. Murtaugh does not replace its
// own binary — where the file lives is the operator's decision, and a package
// manager makes it theirs to automate.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// HTTPGet performs a GET against url and returns the body. Injected so tests
// can stub network access; production wiring is HTTPGetter.
type HTTPGet func(ctx context.Context, url string) ([]byte, error)

// Deps is the explicit dependency bundle passed to New.
type Deps struct {
	CurrentVersion func() string
	HTTPGet        HTTPGet
	Owner, Repo    string
}

// Tool is the `setup.update` capability.
type Tool struct {
	deps Deps
}

// New constructs a Tool from the supplied dependencies.
func New(deps Deps) *Tool { return &Tool{deps: deps} }

// Name returns the registry key.
func (t *Tool) Name() string { return "setup.update" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Report whether a newer Murtaugh release exists and where to read its notes."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"version":          {Type: "string", Description: "Release tag to compare against instead of the latest one."},
			"release_json_url": {Type: "string", Description: "Override the release JSON URL; primarily for local fixtures and tests."},
		},
	}
}

// Result is the structured payload returned by Invoke.
type Result struct {
	CurrentVersion string `json:"current_version"`
	TargetVersion  string `json:"target_version"`
	ReleaseNotes   string `json:"release_notes,omitempty"`
	UpToDate       bool   `json:"up_to_date"`
}

// String renders a one-line CLI answer.
func (r Result) String() string {
	if r.UpToDate {
		return fmt.Sprintf("running %s; that is the latest release", r.CurrentVersion)
	}
	msg := fmt.Sprintf("%s is available (running %s)", r.TargetVersion, r.CurrentVersion)
	if r.ReleaseNotes != "" {
		msg += "\nrelease notes: " + r.ReleaseNotes
	}
	return msg + "\nMurtaugh does not replace its own binary: download the release and put it where this one is."
}

// HTTPGetter returns the default HTTPGet implementation: a plain http.Get
// with a context-aware request. Composition root wires this in.
func HTTPGetter() HTTPGet {
	return func(ctx context.Context, url string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: status %d", url, resp.StatusCode)
		}
		return io.ReadAll(resp.Body)
	}
}

// Invoke compares the running version against a published release.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	target, _ := args["version"].(string)
	override, _ := args["release_json_url"].(string)

	current := t.deps.CurrentVersion()
	if t.deps.HTTPGet == nil {
		return nil, fmt.Errorf("no release source is configured")
	}

	url := strings.TrimSpace(override)
	if url == "" {
		url = releaseURL(t.deps.Owner, t.deps.Repo, target)
	}
	body, err := t.deps.HTTPGet(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("fetch release: %w", err)
	}
	tag, notes, err := parseRelease(body)
	if err != nil {
		return nil, err
	}
	return Result{
		CurrentVersion: current,
		TargetVersion:  tag,
		ReleaseNotes:   notes,
		UpToDate:       equalVersions(current, tag),
	}, nil
}

// releaseURL builds the GitHub API URL for either a specific tag or the
// "latest" release when target is blank.
func releaseURL(owner, repo, target string) string {
	if strings.TrimSpace(target) == "" {
		return fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo)
	}
	return fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/tags/%s", owner, repo, target)
}

// parseRelease pulls the tag and the human-readable release page out of the
// GitHub release JSON.
func parseRelease(body []byte) (string, string, error) {
	var doc struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", "", fmt.Errorf("parse release JSON: %w", err)
	}
	if strings.TrimSpace(doc.TagName) == "" {
		return "", "", fmt.Errorf("the release JSON names no tag")
	}
	return doc.TagName, doc.HTMLURL, nil
}

// equalVersions reports whether two version tags refer to the same release,
// tolerating a leading "v" on either side.
func equalVersions(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}
