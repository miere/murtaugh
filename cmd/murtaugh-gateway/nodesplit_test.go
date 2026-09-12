package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
	configstore "github.com/miere/murtaugh/internal/config/store"
)

// These drive run() — the real command line against a real directory — because
// `cfg node split` was broken on the CLI while its unit tests passed. The fault
// was in the composition root, which an Invoke-level test never reaches.

func TestCfgNodeSplitRunsAgainstARealDestination(t *testing.T) {
	ctx := context.Background()
	gatewayPath := seededGatewayRoot(t)

	nodePath := filepath.Join(t.TempDir(), "node", "config.yaml")

	out := captureStdout(t, func() {
		if err := run([]string{"--config", gatewayPath, "cfg", "node", "split",
			"--dest", nodePath, "--gateway", "wss://gw.example:8443"}); err != nil {
			t.Fatalf("cfg node split --dest: %v", err)
		}
	})

	nodeCfg, nodeStore := loadRoot(t, ctx, nodePath, config.Config{Role: config.RoleNode})
	defer nodeStore.Close()
	if len(nodeCfg.Agents) == 0 {
		t.Errorf("the node root holds no agent profiles; a split that seeds an empty root is the failure this test exists for (%s)", out)
	}
	if _, ok := nodeCfg.Agents["code"]; !ok {
		t.Errorf("the node root does not hold the `code` profile: %v", keysOf(nodeCfg.Agents))
	}
	if len(nodeCfg.Jobs) == 0 || len(nodeCfg.MCPServers) == 0 {
		t.Errorf("the node root holds %d jobs and %d MCP servers, want both populated", len(nodeCfg.Jobs), len(nodeCfg.MCPServers))
	}
	if got := nodeCfg.Node.Gateway; len(got) != 1 || got[0] != "wss://gw.example:8443" {
		t.Errorf("the node's seed address is %v, want [wss://gw.example:8443]", got)
	}

	mint := "murtaugh node token mint --node <id> --user <U…> --token-file " + filepath.Join(filepath.Dir(nodePath), "node-token")
	if !strings.Contains(out, mint) {
		t.Errorf("the split does not name the command that gives the node its credential (%q): %s", mint, out)
	}
	if !strings.Contains(out, "agent 1") {
		t.Errorf("the split does not report copying the agent bodies across: %s", out)
	}
	_, kept, ok := strings.Cut(out, "kept on the gateway:")
	if !ok {
		t.Fatalf("the split reported no gateway half at all: %s", out)
	}
	for _, body := range []string{config.SectionAgent, config.SectionMCP, config.SectionJob} {
		if strings.Contains(kept, body) {
			t.Errorf("the gateway half still counts %q as its own; the bodies are the node's: %s", body, kept)
		}
	}
	if !strings.Contains(kept, config.SingletonAccess) {
		t.Errorf("the gateway half kept no access block, so nothing stayed behind: %s", kept)
	}

	gatewayCfg, gatewayStore := loadRoot(t, ctx, gatewayPath, config.Config{
		Role:  config.RoleGateway,
		OAuth: config.OAuthConfig{AppToken: "xapp-1", BotToken: "xoxb-1"},
	})
	defer gatewayStore.Close()
	if gatewayCfg.Access.AdminUser != "U1" {
		t.Errorf("the gateway lost its access block: %+v", gatewayCfg.Access)
	}
}

func TestCfgNodeSplitValidatesBeforeItWrites(t *testing.T) {
	gatewayPath := seededGatewayRoot(t)
	nodePath := filepath.Join(t.TempDir(), "node", "config.yaml")

	if err := run([]string{"--config", gatewayPath, "cfg", "node", "split",
		"--dest", nodePath, "--gateway", "https://gw.example"}); err == nil {
		t.Fatal("an https:// seed address was accepted")
	}
	if _, err := os.Stat(filepath.Dir(nodePath)); !os.IsNotExist(err) {
		t.Errorf("a refused split seeded %s anyway; the operator's next command finds a root that looks initialised and holds nothing", filepath.Dir(nodePath))
	}
}

func seededGatewayRoot(t *testing.T) string {
	t.Helper()
	t.Setenv("SLACK_APP_TOKEN", "xapp-1")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-1")

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.Bootstrap(path); err != nil {
		t.Fatalf("seed a gateway root: %v", err)
	}
	ctx := context.Background()
	_, store, err := configstore.BootstrapRole(ctx, path, config.RoleGateway, true)
	if err != nil {
		t.Fatalf("open the gateway store: %v", err)
	}
	defer store.Close()

	put := func(section, name string, body any) {
		t.Helper()
		if err := store.UpsertItem(ctx, section, name, body); err != nil {
			t.Fatalf("UpsertItem(%s/%s): %v", section, name, err)
		}
	}
	put(config.SectionAgent, "code", config.AgentProfile{
		Native: &config.NativeProfile{Provider: "anthropic", Model: "claude", APIKeyEnv: "ANTHROPIC_API_KEY"},
	})
	put(config.SectionMCP, "github", config.MCPServerConfig{Command: "gh-mcp"})
	put(config.SectionJob, "nightly", config.JobProfile{Agent: "code", Prompt: "sweep", Schedule: "0 3 * * *"})
	if err := store.PutSingleton(ctx, config.SingletonAccess, config.AccessConfig{AdminUser: "U1"}); err != nil {
		t.Fatalf("PutSingleton(access): %v", err)
	}
	if err := store.PutSingleton(ctx, config.SingletonChat, config.ChatConfig{
		Enabled:  true,
		Defaults: config.ChatDefaults{Agent: "code"},
	}); err != nil {
		t.Fatalf("PutSingleton(chat): %v", err)
	}
	return path
}

func loadRoot(t *testing.T, ctx context.Context, path string, base config.Config) (config.Config, config.Store) {
	t.Helper()
	boot, err := config.LoadBootstrap(path)
	if err != nil {
		t.Fatalf("load bootstrap %s: %v", path, err)
	}
	store, err := configstore.Open(ctx, boot.Database, filepath.Dir(path), config.BaseNameOf(path))
	if err != nil {
		t.Fatalf("open store %s: %v", path, err)
	}
	cfg, err := store.Load(ctx, base)
	if err != nil {
		store.Close()
		t.Fatalf("load %s as %s: %v", path, base.Role, err)
	}
	return cfg, store
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = write
	done := make(chan string, 1)
	go func() {
		out, _ := io.ReadAll(read)
		done <- string(out)
	}()
	func() {
		defer func() {
			os.Stdout = saved
			_ = write.Close()
		}()
		fn()
	}()
	return <-done
}
