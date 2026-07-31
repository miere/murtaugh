package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/store"
)

func newStore(t *testing.T) (config.Store, StoreProvider) {
	t.Helper()
	dbc := config.DatabaseConfig{
		Backend: config.BackendSQLite,
		SQLite:  config.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")},
	}
	s, err := store.Open(context.Background(), dbc, "", "")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, func() (config.Store, error) { return s, nil }
}

func getAgent(t *testing.T, s config.Store, name string) (config.AgentProfile, bool) {
	t.Helper()
	body, ok, err := s.GetItem(context.Background(), config.SectionAgent, name)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if !ok {
		return config.AgentProfile{}, false
	}
	var p config.AgentProfile
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("unmarshal agent: %v", err)
	}
	return p, true
}

func TestTool_Metadata(t *testing.T) {
	_, prov := newStore(t)
	tl := New(prov, nil)
	if tl.Name() != "setup.agents" {
		t.Fatalf("Name() = %q, want setup.agents", tl.Name())
	}
	if tl.InputSchema() == nil {
		t.Fatal("InputSchema must not be nil")
	}
}

func TestInvoke_NoCommandSetsDefaultsOnly(t *testing.T) {
	s, prov := newStore(t)
	tl := New(prov, nil)

	res, err := tl.Invoke(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.(Result).Enabled {
		t.Fatal("Enabled must be false when no agent is configured")
	}
	// Runtime defaults were established.
	body, ok, err := s.GetSingleton(context.Background(), config.SingletonDefaults)
	if err != nil || !ok {
		t.Fatalf("defaults singleton missing: ok=%v err=%v", ok, err)
	}
	var d config.RuntimeDefaults
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(err)
	}
	if d.ACP.StartupTimeout != "10s" || d.Session.MaxConcurrent != 100 {
		t.Fatalf("runtime defaults wrong: %+v", d)
	}
	// No agent row was written.
	if list, _ := s.ListItems(context.Background(), config.SectionAgent); len(list) != 0 {
		t.Fatalf("no agent expected, got %v", list)
	}
}

func TestInvoke_WithCommandRegistersACPAgent(t *testing.T) {
	s, prov := newStore(t)
	tl := New(prov, nil)

	res, err := tl.Invoke(context.Background(), map[string]any{
		"command": "/usr/local/bin/auggie",
		"args":    []any{"--acp", "--allow-indexing"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.(Result).Enabled {
		t.Fatal("Enabled must be true when an agent is configured")
	}
	agent, ok := getAgent(t, s, "default")
	if !ok {
		t.Fatal("default agent missing")
	}
	if agent.ACP == nil || agent.ACP.Command != "/usr/local/bin/auggie" {
		t.Fatalf("command wrong: %+v", agent.ACP)
	}
	if len(agent.ACP.Args) != 2 || agent.ACP.Args[0] != "--acp" {
		t.Fatalf("args = %v", agent.ACP.Args)
	}
}

func TestInvoke_CustomAgentNameIsHonoured(t *testing.T) {
	s, prov := newStore(t)
	tl := New(prov, nil)

	if _, err := tl.Invoke(context.Background(), map[string]any{
		"agent_name": "ccode",
		"command":    "/usr/local/bin/claude",
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if _, ok := getAgent(t, s, "ccode"); !ok {
		t.Fatal("agents[ccode] missing")
	}
}

func TestInvoke_PreservesCustomisedDefaults(t *testing.T) {
	s, prov := newStore(t)
	// An operator-customised defaults singleton must not be clobbered on re-run.
	custom := config.RuntimeDefaults{Session: config.SessionDefaults{MaxConcurrent: 7}}
	if err := s.PutSingleton(context.Background(), config.SingletonDefaults, custom); err != nil {
		t.Fatal(err)
	}
	tl := New(prov, nil)
	if _, err := tl.Invoke(context.Background(), map[string]any{"command": "/bin/x"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	body, _, _ := s.GetSingleton(context.Background(), config.SingletonDefaults)
	var d config.RuntimeDefaults
	_ = json.Unmarshal(body, &d)
	if d.Session.MaxConcurrent != 7 {
		t.Fatalf("customised defaults were clobbered: %+v", d)
	}
}

func TestInvoke_NativeAgent(t *testing.T) {
	s, prov := newStore(t)
	tl := New(prov, nil)

	res, err := tl.Invoke(context.Background(), map[string]any{
		"agent_name":      "emily",
		"provider":        "gemini",
		"model":           "gemini-2.5-pro",
		"api_key_env":     "GEMINI_API_KEY",
		"tools":           []any{"files", "terminal", "skills"},
		"mcp_servers":     []any{"vaultre"},
		"context_limit":   "200000",
		"compaction":      "summarize",
		"cache_retention": "1h",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if !r.Enabled || r.Kind != "native" || r.AgentName != "emily" {
		t.Fatalf("unexpected result: %+v", r)
	}
	a, ok := getAgent(t, s, "emily")
	if !ok {
		t.Fatal("native agent missing")
	}
	if a.Native == nil || a.Native.Provider != "gemini" || a.Native.Model != "gemini-2.5-pro" || a.Native.APIKeyEnv != "GEMINI_API_KEY" {
		t.Fatalf("native fields wrong: %+v", a.Native)
	}
	if a.ACP != nil {
		t.Errorf("native profile must not carry an acp block")
	}
	if a.Native.ContextLimit != 200000 || a.Native.Compaction != "summarize" || a.Native.CacheRetention != "1h" {
		t.Errorf("native tuning wrong: %+v", a.Native)
	}
	if len(a.Tools) != 3 || len(a.MCPServers) != 1 {
		t.Errorf("tools/mcp_servers wrong: %+v", a)
	}
}

func TestInvoke_NativeValidation(t *testing.T) {
	_, prov := newStore(t)
	tl := New(prov, nil)
	cases := []map[string]any{
		{"provider": "gemini", "model": "m"},
		{"provider": "gemini", "api_key_env": "K"},
		{"kind": "native", "model": "m", "api_key_env": "K"},
		{"provider": "cohere", "model": "m", "api_key_env": "K"},
		{"provider": "gemini", "model": "m", "api_key_env": "K", "compaction": "shrink"},
		{"provider": "gemini", "model": "m", "api_key_env": "K", "cache_retention": "2h"},
	}
	for i, args := range cases {
		if _, err := tl.Invoke(context.Background(), args); err == nil {
			t.Errorf("case %d: expected error for %+v", i, args)
		}
	}
}

func TestInvoke_RejectsArgsWithoutCommand(t *testing.T) {
	_, prov := newStore(t)
	tl := New(prov, nil)
	if _, err := tl.Invoke(context.Background(), map[string]any{"args": []any{"--foo"}}); err == nil {
		t.Fatal("Invoke should reject args without command")
	}
}

// loadedTroubleshoot reads the providers list from a troubleshoot.yaml.
type loadedTroubleshoot struct {
	Troubleshoot struct {
		Providers []string `yaml:"providers"`
	} `yaml:"troubleshoot"`
}

func TestInvoke_ClaudeCodeAgentRecordsTroubleshootProvider(t *testing.T) {
	s, prov := newStore(t)
	tsPath := filepath.Join(t.TempDir(), "troubleshoot.yaml")
	tl := New(prov, func() string { return tsPath })

	res, err := tl.Invoke(context.Background(), map[string]any{
		"agent_name": "carmen",
		"kind":       "claude_code",
		"command":    "/Users/x/.local/bin/claude",
		"model":      "claude-opus-4-8",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	if !r.Enabled || r.Kind != "claude_code" {
		t.Fatalf("unexpected result: %+v", r)
	}
	if !r.ProviderRecorded {
		t.Fatal("ProviderRecorded must be true on first claude_code write")
	}
	if r.Warning != "" {
		t.Fatalf("unexpected warning: %s", r.Warning)
	}

	agent, ok := getAgent(t, s, "carmen")
	if !ok {
		t.Fatal("claude_code agent missing")
	}
	if agent.ClaudeCode == nil || agent.ClaudeCode.Command != "/Users/x/.local/bin/claude" || agent.ClaudeCode.Model != "claude-opus-4-8" {
		t.Fatalf("claude_code fields wrong: %+v", agent.ClaudeCode)
	}
	if agent.ACP != nil || agent.Native != nil {
		t.Errorf("claude_code profile must not carry acp/native blocks: %+v", agent)
	}

	// The provider must be recorded so troubleshoot bundles pick up ~/.claude.
	tsData, err := os.ReadFile(tsPath)
	if err != nil {
		t.Fatalf("read troubleshoot.yaml: %v", err)
	}
	var ts loadedTroubleshoot
	if err := yaml.Unmarshal(tsData, &ts); err != nil {
		t.Fatalf("parse troubleshoot.yaml: %v", err)
	}
	found := false
	for _, p := range ts.Troubleshoot.Providers {
		if p == "claude-code" {
			found = true
		}
	}
	if !found {
		t.Fatalf("claude-code not recorded in troubleshoot providers: %v", ts.Troubleshoot.Providers)
	}

	// A second claude_code write must not duplicate the provider entry.
	res2, err := tl.Invoke(context.Background(), map[string]any{
		"agent_name": "carmen", "kind": "claude_code", "command": "/Users/x/.local/bin/claude",
	})
	if err != nil {
		t.Fatalf("second Invoke: %v", err)
	}
	if res2.(Result).ProviderRecorded {
		t.Error("ProviderRecorded must be false when the provider was already listed")
	}
}

func TestInvoke_ClaudeCodeRequiresCommand(t *testing.T) {
	_, prov := newStore(t)
	tl := New(prov, nil)
	if _, err := tl.Invoke(context.Background(), map[string]any{"kind": "claude_code"}); err == nil {
		t.Fatal("kind claude_code without --command must error")
	}
}

// A bare --command still means acp: claude_code is opt-in via --kind only.
func TestInvoke_ClaudeCodeIsNotInferredFromCommand(t *testing.T) {
	s, prov := newStore(t)
	tl := New(prov, nil)
	if _, err := tl.Invoke(context.Background(), map[string]any{"command": "/usr/local/bin/claude"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	agent, ok := getAgent(t, s, "default")
	if !ok {
		t.Fatal("default agent missing")
	}
	if agent.ACP == nil || agent.ClaudeCode != nil {
		t.Fatalf("a bare --command must infer acp, not claude_code: %+v", agent)
	}
}
