package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

type countingTransport struct {
	next  http.RoundTripper
	calls atomic.Int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return c.next.RoundTrip(r)
}

func canvasServer(t *testing.T, reply string) (*httptest.Server, *url.Values, *http.Header) {
	t.Helper()
	var form url.Values
	var header http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/canvases.edit" {
			t.Errorf("request to %s, want /canvases.edit", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(body))
		header = r.Header.Clone()
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv, &form, &header
}

func testClient(t *testing.T, srv *httptest.Server, rt *countingTransport) *SlackClient {
	t.Helper()
	c, err := NewClientWithHTTP("xoxb-test", &http.Client{Transport: rt})
	if err != nil {
		t.Fatalf("NewClientWithHTTP: %v", err)
	}
	c.endpoint = srv.URL + "/"
	return c
}

// Slack rejects a delete carrying document_content, which slack-go always sends.
func TestEditCanvas_DeleteSendsNoDocumentContent(t *testing.T) {
	srv, form, header := canvasServer(t, `{"ok":true}`)
	rt := &countingTransport{next: http.DefaultTransport}
	c := testClient(t, srv, rt)

	err := c.EditCanvas(context.Background(), CanvasEditParams{
		CanvasID:  "F0BLC4MF7QU",
		SectionID: "temp:C:abc123",
		Operation: "delete",
	})
	if err != nil {
		t.Fatalf("EditCanvas(delete): %v", err)
	}

	if got := form.Get("canvas_id"); got != "F0BLC4MF7QU" {
		t.Errorf("canvas_id = %q", got)
	}
	raw := form.Get("changes")
	if strings.Contains(raw, "document_content") {
		t.Fatalf("delete payload carries document_content, which Slack rejects: %s", raw)
	}
	var changes []map[string]any
	if err := json.Unmarshal([]byte(raw), &changes); err != nil {
		t.Fatalf("changes is not JSON: %v (%s)", err, raw)
	}
	want := map[string]any{"operation": "delete", "section_id": "temp:C:abc123"}
	if len(changes) != 1 || len(changes[0]) != len(want) ||
		changes[0]["operation"] != want["operation"] || changes[0]["section_id"] != want["section_id"] {
		t.Fatalf("changes = %v, want exactly [%v]", changes, want)
	}
	if got := header.Get("Authorization"); got != "Bearer xoxb-test" {
		t.Errorf("Authorization = %q, want the bot token", got)
	}
}

// A node that lost the leader election is only silenced if every call uses this transport.
func TestEditCanvas_DeleteUsesTheInjectedTransport(t *testing.T) {
	srv, _, _ := canvasServer(t, `{"ok":true}`)
	rt := &countingTransport{next: http.DefaultTransport}
	c := testClient(t, srv, rt)

	if err := c.EditCanvas(context.Background(), CanvasEditParams{CanvasID: "F1", SectionID: "s1", Operation: "delete"}); err != nil {
		t.Fatalf("EditCanvas(delete): %v", err)
	}
	if n := rt.calls.Load(); n != 1 {
		t.Fatalf("injected transport saw %d requests, want 1 — the delete bypassed the gate", n)
	}
}

func TestEditCanvas_DeleteSurfacesSlackErrors(t *testing.T) {
	srv, _, _ := canvasServer(t, `{"ok":false,"error":"missing_scope"}`)
	c := testClient(t, srv, &countingTransport{next: http.DefaultTransport})

	err := c.EditCanvas(context.Background(), CanvasEditParams{CanvasID: "F1", SectionID: "s1", Operation: "delete"})
	if err == nil {
		t.Fatal("EditCanvas(delete) swallowed ok:false")
	}
	if !strings.HasPrefix(err.Error(), "Slack error (canvases.edit): missing_scope") {
		t.Fatalf("err = %q, want the method and code", err)
	}
	if !strings.Contains(err.Error(), "retrying will not help") {
		t.Errorf("err = %q, want the missing_scope hint", err)
	}
}

func TestEditCanvas_DeleteReportsAnUndecodableResponse(t *testing.T) {
	srv, _, _ := canvasServer(t, `<html>gateway timeout</html>`)
	c := testClient(t, srv, &countingTransport{next: http.DefaultTransport})

	err := c.EditCanvas(context.Background(), CanvasEditParams{CanvasID: "F1", SectionID: "s1", Operation: "delete"})
	if err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("err = %v, want a decode failure rather than a silent success", err)
	}
}
