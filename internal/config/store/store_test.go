package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

// openTestStore opens a fresh SQLite config store in a temp dir.
func openTestStore(t *testing.T) config.Store {
	t.Helper()
	dbc := config.DatabaseConfig{
		Backend: config.BackendSQLite,
		SQLite:  config.SQLiteConfig{Path: filepath.Join(t.TempDir(), "config.db")},
	}
	s, err := Open(context.Background(), dbc)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// baseConfig is the file-sourced portion Load carries through (OAuth is required
// by Validate; everything else comes from the store).
func baseConfig() config.Config {
	return config.Config{
		BaseDir: "/tmp/murtaugh",
		OAuth:   config.OAuthConfig{AppToken: "xapp-test", BotToken: "xoxb-test"},
	}
}

func nativeAgent() config.AgentProfile {
	return config.AgentProfile{
		Tools:  []string{"files", "terminal"},
		Native: &config.NativeProfile{Provider: "gemini", Model: "gemini-2.5-pro", APIKeyEnv: "GEMINI_API_KEY"},
	}
}

func TestItemCRUD(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertItem(ctx, config.SectionAgent, "code", nativeAgent()); err != nil {
		t.Fatalf("UpsertItem: %v", err)
	}
	body, ok, err := s.GetItem(ctx, config.SectionAgent, "code")
	if err != nil || !ok {
		t.Fatalf("GetItem: ok=%v err=%v", ok, err)
	}
	var got config.AgentProfile
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Native == nil || got.Native.Model != "gemini-2.5-pro" {
		t.Fatalf("round-trip lost native profile: %+v", got)
	}

	// Update replaces in place.
	updated := nativeAgent()
	updated.Native.Model = "gemini-3-pro"
	if err := s.UpsertItem(ctx, config.SectionAgent, "code", updated); err != nil {
		t.Fatalf("update: %v", err)
	}
	list, err := s.ListItems(ctx, config.SectionAgent)
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 agent after update, got %d", len(list))
	}

	deleted, err := s.DeleteItem(ctx, config.SectionAgent, "code")
	if err != nil || !deleted {
		t.Fatalf("DeleteItem: deleted=%v err=%v", deleted, err)
	}
	if _, ok, _ := s.GetItem(ctx, config.SectionAgent, "code"); ok {
		t.Fatalf("item still present after delete")
	}
	// Deleting a missing item reports false, not an error.
	if deleted, err := s.DeleteItem(ctx, config.SectionAgent, "nope"); err != nil || deleted {
		t.Fatalf("delete missing: deleted=%v err=%v", deleted, err)
	}
}

func TestUnknownSectionRejected(t *testing.T) {
	s := openTestStore(t)
	if err := s.UpsertItem(context.Background(), "bogus", "x", nativeAgent()); err == nil {
		t.Fatal("expected error for unknown section")
	}
	if err := s.PutSingleton(context.Background(), "bogus", struct{}{}); err == nil {
		t.Fatal("expected error for unknown singleton")
	}
}

func TestLoadAssemblesValidatedConfig(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.UpsertItem(ctx, config.SectionAgent, "code", nativeAgent()); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertItem(ctx, config.SectionMCP, "vault", config.MCPServerConfig{URL: "https://mcp.example"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertItem(ctx, config.SectionJob, "nightly", config.JobProfile{Command: "echo", Schedule: "0 2 * * *"}); err != nil {
		t.Fatal(err)
	}
	chat := config.ChatConfig{Enabled: true, Defaults: config.ChatDefaults{Agent: "code"}}
	if err := s.PutSingleton(ctx, config.SingletonChat, chat); err != nil {
		t.Fatal(err)
	}
	access := config.AccessConfig{AdminUser: "U123", AllowedUsers: []string{"U456"}}
	if err := s.PutSingleton(ctx, config.SingletonAccess, access); err != nil {
		t.Fatal(err)
	}

	cfg, err := s.Load(ctx, baseConfig())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OAuth.AppToken != "xapp-test" {
		t.Errorf("base OAuth not carried through: %q", cfg.OAuth.AppToken)
	}
	if _, ok := cfg.Agents["code"]; !ok {
		t.Errorf("agent not loaded: %+v", cfg.Agents)
	}
	if !cfg.Chat.Enabled || cfg.Chat.Defaults.Agent != "code" {
		t.Errorf("chat singleton not loaded: %+v", cfg.Chat)
	}
	if cfg.Access.AdminUser != "U123" {
		t.Errorf("access singleton not loaded: %+v", cfg.Access)
	}
	if len(cfg.Jobs) != 1 || len(cfg.MCPServers) != 1 {
		t.Errorf("jobs/mcp not loaded: jobs=%d mcp=%d", len(cfg.Jobs), len(cfg.MCPServers))
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// chat enabled with an unknown default agent → Validate must fail.
	chat := config.ChatConfig{Enabled: true, Defaults: config.ChatDefaults{Agent: "missing"}}
	if err := s.PutSingleton(ctx, config.SingletonChat, chat); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(ctx, baseConfig()); err == nil {
		t.Fatal("expected Load to fail validation for unknown chat agent")
	}
}

func TestWorkflowRuleTriggerRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.UpsertItem(ctx, config.SectionAgent, "code", nativeAgent()); err != nil {
		t.Fatal(err)
	}
	rule := config.WorkflowRuleConfig{
		RequestEvent: "interactive",
		Match:        map[string]any{"callback_id": "deploy"},
		Triggers: []config.TriggerConfig{
			{Type: "delegate-to-agent", DelegateToAgent: &config.DelegateToAgentConfig{Agent: "code", Prompt: "go"}},
		},
	}
	if err := s.UpsertItem(ctx, config.SectionWorkflowRule, "deploy", rule); err != nil {
		t.Fatalf("upsert rule: %v", err)
	}
	cfg, err := s.Load(ctx, baseConfig())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.WorkflowRules["deploy"]
	if len(got.Triggers) != 1 {
		t.Fatalf("trigger lost: %+v", got)
	}
	tr := got.Triggers[0]
	if tr.Type != "delegate-to-agent" || tr.DelegateToAgent == nil || tr.DelegateToAgent.Agent != "code" {
		t.Fatalf("polymorphic trigger did not round-trip: %+v", tr)
	}
}

func TestTriggerJSONShape(t *testing.T) {
	tr := config.TriggerConfig{Type: "run", Run: &config.RunTriggerConfig{Cmd: "deploy.sh"}}
	raw, err := json.Marshal(tr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The trigger must serialize as a single-key object keyed by its action.
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("shape: %v", err)
	}
	if len(shape) != 1 {
		t.Fatalf("want single-key object, got %s", raw)
	}
	if _, ok := shape["run"]; !ok {
		t.Fatalf("want key %q, got %s", "run", raw)
	}
	var back config.TriggerConfig
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Type != "run" || back.Run == nil || back.Run.Cmd != "deploy.sh" {
		t.Fatalf("did not round-trip: %+v", back)
	}
}

func TestSnapshotRestoreParity(t *testing.T) {
	src := openTestStore(t)
	ctx := context.Background()
	if err := src.UpsertItem(ctx, config.SectionAgent, "code", nativeAgent()); err != nil {
		t.Fatal(err)
	}
	if err := src.UpsertItem(ctx, config.SectionJob, "nightly", config.JobProfile{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	if err := src.PutSingleton(ctx, config.SingletonAccess, config.AccessConfig{AdminUser: "U1"}); err != nil {
		t.Fatal(err)
	}
	snap, err := src.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	dst := openTestStore(t)
	if err := dst.Restore(ctx, snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	srcCfg, err := src.Load(ctx, baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	dstCfg, err := dst.Load(ctx, baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(srcCfg, dstCfg) {
		t.Fatalf("restored config differs:\n src=%+v\n dst=%+v", srcCfg, dstCfg)
	}
}
