package cfg

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/store"
	"github.com/miere/murtaugh/internal/tools"
)

func testProvider(t *testing.T) Provider {
	t.Helper()
	dbc := config.DatabaseConfig{
		Backend: config.BackendSQLite,
		SQLite:  config.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")},
	}
	s, err := store.Open(context.Background(), dbc)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return NewProvider(s)
}

func find(t *testing.T, list []tools.Tool, name string) tools.Tool {
	t.Helper()
	for _, tl := range list {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func invoke(t *testing.T, tl tools.Tool, args map[string]any) (any, error) {
	t.Helper()
	return tl.Invoke(context.Background(), args)
}

func TestCfgAgentLifecycle(t *testing.T) {
	p := testProvider(t)
	agents := AgentTools(p)
	singles := SingletonTools(p)
	admin := AdminTools(p)

	// create
	if _, err := invoke(t, find(t, agents, "cfg.agent.create"), map[string]any{
		"name":    "code",
		"type":    "claude-code",
		"command": "claude",
		"env":     []any{"ANTHROPIC_MODEL=opus"},
	}); err != nil {
		t.Fatalf("agent create: %v", err)
	}
	// duplicate create fails
	if _, err := invoke(t, find(t, agents, "cfg.agent.create"), map[string]any{
		"name": "code", "type": "native", "provider": "gemini", "model": "g", "api_key_env": "K",
	}); err == nil {
		t.Fatal("duplicate agent create should fail")
	}
	// list
	res, err := invoke(t, find(t, agents, "cfg.agent.list"), nil)
	if err != nil {
		t.Fatalf("agent list: %v", err)
	}
	if lr, ok := res.(listResult); !ok || len(lr.Names) != 1 || lr.Names[0] != "code" {
		t.Fatalf("agent list = %+v", res)
	}
	// show carries the claude_code block
	res, err = invoke(t, find(t, agents, "cfg.agent.show"), map[string]any{"name": "code"})
	if err != nil {
		t.Fatalf("agent show: %v", err)
	}
	if !strings.Contains(string(res.(showResult).Body), "claude_code") {
		t.Fatalf("show body missing claude_code: %s", res.(showResult).Body)
	}

	// wire chat to the agent, then validate
	if _, err := invoke(t, find(t, singles, "cfg.chat.set"), map[string]any{
		"enabled": true, "default_agent": "code",
	}); err != nil {
		t.Fatalf("chat set: %v", err)
	}
	if _, err := invoke(t, find(t, admin, "cfg.validate"), nil); err != nil {
		t.Fatalf("validate: %v", err)
	}

	// deleting the agent chat points at must be rejected (validation rollback)
	if _, err := invoke(t, find(t, agents, "cfg.agent.delete"), map[string]any{"name": "code"}); err == nil {
		t.Fatal("deleting a referenced agent should be rejected")
	}
	// the agent is still there after the rejected delete
	if _, err := invoke(t, find(t, agents, "cfg.agent.show"), map[string]any{"name": "code"}); err != nil {
		t.Fatalf("agent should survive rejected delete: %v", err)
	}

	// dump includes agent + chat
	res, err = invoke(t, find(t, admin, "cfg.show"), nil)
	if err != nil {
		t.Fatalf("cfg show: %v", err)
	}
	dump := res.(dumpResult).String()
	if !strings.Contains(dump, "code") || !strings.Contains(dump, "chat") {
		t.Fatalf("dump missing content:\n%s", dump)
	}
}

func TestCfgAgentUpdateMergesFields(t *testing.T) {
	p := testProvider(t)
	agents := AgentTools(p)
	if _, err := invoke(t, find(t, agents, "cfg.agent.create"), map[string]any{
		"name": "n", "type": "native", "provider": "gemini", "model": "gemini-2.5-pro", "api_key_env": "K",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// update only the model; provider/api_key_env must be preserved.
	if _, err := invoke(t, find(t, agents, "cfg.agent.update"), map[string]any{
		"name": "n", "model": "gemini-3-pro",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	s, _ := p()
	body, _, _ := s.GetItem(context.Background(), config.SectionAgent, "n")
	str := string(body)
	if !strings.Contains(str, "gemini-3-pro") || !strings.Contains(str, "gemini") || !strings.Contains(str, "\"api_key_env\":\"K\"") {
		t.Fatalf("update did not merge fields: %s", str)
	}
}

func TestCfgRejectsInvalidCreate(t *testing.T) {
	p := testProvider(t)
	agents := AgentTools(p)
	// native without provider/model → Validate fails → create rolled back.
	if _, err := invoke(t, find(t, agents, "cfg.agent.create"), map[string]any{
		"name": "bad", "type": "native",
	}); err == nil {
		t.Fatal("invalid native agent create should be rejected")
	}
	// nothing persisted
	res, _ := invoke(t, find(t, agents, "cfg.agent.list"), nil)
	if len(res.(listResult).Names) != 0 {
		t.Fatalf("rolled-back create left rows: %+v", res)
	}
}
