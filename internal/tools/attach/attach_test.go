package attach

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/tools/files"
)

func newRoot(t *testing.T, dir string) *files.Root {
	t.Helper()
	root, err := files.NewRoot(dir)
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	return root
}

func TestInvoke_ReturnsAttachment(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A workspace-relative path resolves under the root.
	res, err := New(newRoot(t, dir)).Invoke(context.Background(), map[string]any{
		"path": "out.txt", "title": "Report", "comment": "see attached",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	a, ok := res.(*agent.AttachmentEvent)
	if !ok {
		t.Fatalf("result type %T, want *agent.AttachmentEvent", res)
	}
	if a.Path != filepath.Join(dir, "out.txt") || a.Filename != "out.txt" || a.Title != "Report" || a.Comment != "see attached" {
		t.Fatalf("attachment = %+v", a)
	}
	if len(a.Data) != 0 {
		t.Fatalf("attach must not buffer bytes; got %d", len(a.Data))
	}
}

func TestInvoke_RejectsPathOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	// A real, non-empty secret outside the workspace.
	secret := filepath.Join(t.TempDir(), "secret.env")
	if err := os.WriteFile(secret, []byte("API_KEY=xyz"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := New(newRoot(t, dir))

	for _, p := range []string{secret, "../" + filepath.Base(secret), "/etc/passwd"} {
		if _, err := tool.Invoke(context.Background(), map[string]any{"path": p}); err == nil {
			t.Fatalf("expected rejection for path escaping root: %q", p)
		}
	}
}

func TestInvoke_Errors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tool := New(newRoot(t, dir))

	cases := map[string]map[string]any{
		"missing path": {},
		"nonexistent":  {"path": "nope"},
		"directory":    {"path": "."},
		"empty file":   {"path": "empty"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := tool.Invoke(context.Background(), args); err == nil {
				t.Fatalf("expected error for %s", name)
			}
		})
	}
}

func TestNew_NilRootInvokeErrors(t *testing.T) {
	if _, err := New(nil).Invoke(context.Background(), map[string]any{"path": "x"}); err == nil {
		t.Fatal("expected error when no root is configured")
	}
}

// A result returned over MCP only reaches the model, so behind the bridge the file
// must go onto the turn instead, and the model gets a confirmation, not the struct.
func TestInvoke_BehindTheBridgeEmitsOntoTheTurn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("# findings"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []agent.Event
	ctx := agent.WithTurnEmitter(context.Background(), func(ev agent.Event) bool {
		got = append(got, ev)
		return true
	})

	res, err := New(newRoot(t, dir)).Invoke(ctx, map[string]any{"path": "report.md", "title": "Findings"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(got) != 1 || got[0].Type != agent.EventAttachment || got[0].Attachment == nil {
		t.Fatalf("turn received %+v, want one attachment", got)
	}
	if a := got[0].Attachment; a.Path != filepath.Join(dir, "report.md") || a.Title != "Findings" {
		t.Fatalf("attachment = %+v", a)
	}
	if msg, ok := res.(string); !ok || msg != "Attached report.md (10 bytes) to your reply." {
		t.Fatalf("result = %#v, want a one-line confirmation", res)
	}
}

func TestInvoke_BehindTheBridgeWithNoTurnFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("# findings"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := agent.WithTurnEmitter(context.Background(), func(agent.Event) bool { return false })

	if _, err := New(newRoot(t, dir)).Invoke(ctx, map[string]any{"path": "report.md"}); err == nil {
		t.Fatal("claimed success with no conversation to attach to")
	}
}
