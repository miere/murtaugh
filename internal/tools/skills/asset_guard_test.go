package skills_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/assets"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools/skills"
)

// materializeSkills copies the embedded skills/ tree to a temp dir and returns
// its path, so the gating logic can run against the real shipped assets.
func materializeSkills(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	err := fs.WalkDir(assets.FS, "skills", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, "skills/")
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := assets.FS.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("materialize skills: %v", err)
	}
	return dst
}

// TestMurtaughSlack_NoManageLeakToChatAgent is the maintainability guard from
// the design: a typical chat agent (slack/ask, no `manage`) must never receive
// the operator sections of the merged murtaugh-slack skill — not in the rendered
// body, not in the file inventory, and not via a direct file fetch.
func TestMurtaughSlack_NoManageLeakToChatAgent(t *testing.T) {
	dir := materializeSkills(t)
	chat := skills.New(dir, "slack", "ask", "present_plan", "files", "terminal", "skills")

	got, err := chat.Invoke(context.Background(), map[string]any{"name": "murtaugh-slack"})
	if err != nil {
		t.Fatalf("read murtaugh-slack: %v", err)
	}
	res := got.(skills.ReadResult)

	// The rendered body must not mention the operator (manage) files at all.
	for _, leaked := range []string{"workflow-rules.md", "unfurl.md", "automations.md"} {
		if strings.Contains(res.Content, leaked) {
			t.Errorf("manage section %q leaked into a chat agent's body:\n%s", leaked, res.Content)
		}
	}
	// But the runtime rows must be present.
	for _, want := range []string{"messaging.md", "asking.md", "blocks.md"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("runtime row %q missing from body:\n%s", want, res.Content)
		}
	}
	// Inventory must exclude the manage files.
	for _, f := range res.Files {
		if strings.Contains(f, "workflow-rules") || strings.Contains(f, "unfurl") || strings.Contains(f, "automations") {
			t.Errorf("manage file %q leaked into inventory: %v", f, res.Files)
		}
	}
	// Direct fetch of a manage file is refused.
	if _, err := chat.Invoke(context.Background(), map[string]any{"name": "murtaugh-slack", "file": "reference/workflow-rules.md"}); err == nil {
		t.Error("chat agent fetched reference/workflow-rules.md — gate bypassed")
	}

	// A manage agent, by contrast, sees the operator sections.
	op := skills.New(dir, "slack", "manage")
	gotOp, err := op.Invoke(context.Background(), map[string]any{"name": "murtaugh-slack"})
	if err != nil {
		t.Fatalf("read as manage: %v", err)
	}
	if !strings.Contains(gotOp.(skills.ReadResult).Content, "workflow-rules.md") {
		t.Errorf("manage agent should see the workflow-rules row:\n%s", gotOp.(skills.ReadResult).Content)
	}
	if _, err := op.Invoke(context.Background(), map[string]any{"name": "murtaugh-slack", "file": "reference/unfurl.md"}); err != nil {
		t.Errorf("manage agent could not read reference/unfurl.md: %v", err)
	}
}

// TestAllShippedSkills_FrontmatterParses sanity-checks that every shipped skill's
// frontmatter is well-formed enough to list (no parse panic, names resolve).
func TestAllShippedSkills_FrontmatterParses(t *testing.T) {
	dir := materializeSkills(t)
	// A superset agent should see every shipped skill.
	all := skills.New(dir, "slack", "ask", "present_plan", "jobs", "journal", "setup", "restart", "manage", "files", "terminal", "skills")
	got, err := all.Invoke(context.Background(), nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	res := got.(skills.ListResult)
	if len(res.Skills) < 7 {
		t.Errorf("expected the 7 shipped skills, got %d: %+v", len(res.Skills), res.Skills)
	}
	for _, s := range res.Skills {
		if s.Name == "" || s.Description == "" {
			t.Errorf("skill with empty name/description: %+v", s)
		}
	}
}
