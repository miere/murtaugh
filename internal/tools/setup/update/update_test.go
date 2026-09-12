package update

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type stubServer struct {
	releaseJSON []byte
	err         error
	calls       []string
}

func (s *stubServer) get(_ context.Context, url string) ([]byte, error) {
	s.calls = append(s.calls, url)
	return s.releaseJSON, s.err
}

func releaseFor(t *testing.T, tag string) []byte {
	t.Helper()
	out, err := json.Marshal(map[string]any{
		"tag_name": tag,
		"html_url": "https://github.com/miere/murtaugh/releases/tag/" + tag,
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func newTool(current string, srv *stubServer) *Tool {
	return New(Deps{
		CurrentVersion: func() string { return current },
		HTTPGet:        srv.get,
		Owner:          "miere",
		Repo:           "murtaugh",
	})
}

func TestTool_Metadata(t *testing.T) {
	tl := newTool("v1.0.0", &stubServer{})
	if tl.Name() != "setup.update" {
		t.Fatalf("Name() = %q", tl.Name())
	}
	if tl.InputSchema() == nil {
		t.Fatal("InputSchema must not be nil")
	}
}

func TestInvoke_SameVersionIsUpToDate(t *testing.T) {
	tl := newTool("v1.0.0", &stubServer{releaseJSON: releaseFor(t, "v1.0.0")})
	res, err := tl.Invoke(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if r := res.(Result); !r.UpToDate {
		t.Fatalf("UpToDate = false for the same version: %+v", r)
	}
}

// The whole point of the thinned tool: it reports, it does not fetch a binary.
func TestInvoke_NewerReleasePointsAtTheNotes(t *testing.T) {
	srv := &stubServer{releaseJSON: releaseFor(t, "v2.0.0")}
	tl := newTool("v1.0.0", srv)
	res, err := tl.Invoke(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if r.UpToDate || r.TargetVersion != "v2.0.0" {
		t.Fatalf("unexpected result: %+v", r)
	}
	if !strings.Contains(r.ReleaseNotes, "releases/tag/v2.0.0") {
		t.Errorf("no release notes link: %+v", r)
	}
	if len(srv.calls) != 1 {
		t.Errorf("the tool made %d requests; a report needs exactly one: %v", len(srv.calls), srv.calls)
	}
	if !strings.Contains(r.String(), "does not replace its own binary") {
		t.Errorf("the answer does not say who installs the release: %s", r.String())
	}
}

func TestInvoke_DevBuildStillReports(t *testing.T) {
	tl := newTool("dev", &stubServer{releaseJSON: releaseFor(t, "v2.0.0")})
	res, err := tl.Invoke(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("a dev build cannot be clobbered any more, so the check must still answer: %v", err)
	}
	if r := res.(Result); r.UpToDate {
		t.Errorf("a dev build reported as up to date: %+v", r)
	}
}

func TestInvoke_NamedVersionAsksForThatTag(t *testing.T) {
	srv := &stubServer{releaseJSON: releaseFor(t, "v1.5.0")}
	if _, err := newTool("v1.0.0", srv).Invoke(context.Background(), map[string]any{"version": "v1.5.0"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(srv.calls) != 1 || !strings.HasSuffix(srv.calls[0], "/releases/tags/v1.5.0") {
		t.Fatalf("requested %v, want the tagged release", srv.calls)
	}
}

func TestInvoke_FetchFailureSurfaces(t *testing.T) {
	tl := newTool("v1.0.0", &stubServer{err: errors.New("offline")})
	if _, err := tl.Invoke(context.Background(), map[string]any{}); err == nil {
		t.Fatal("a failed fetch was reported as success")
	}
}
