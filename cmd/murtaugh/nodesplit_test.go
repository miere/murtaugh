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

// `murtaugh cfg node split` is the only path #198 offers for turning a combined
// install into two halves, and until this test existed nobody had run it.
//
// The tests below it exercised store.SplitForNode directly, which is the half
// that always worked. What did not work was the composition root: the
// destination flag was called `--config`, and `--config` is the GLOBAL flag that
// cmd/murtaugh strips from the whole command line before any tool is dispatched.
// So the documented invocation
//
//	murtaugh --config <gateway> cfg node split --config <node>
//
// ran the ENTIRE command against the node path — bootstrapping a gateway
// skeleton there, so the destination looked initialised and held nothing — while
// the tool itself saw no argument and aimed its output at
// config.DefaultNodePath(). Every layer behaved exactly as documented and the
// command could not work.
//
// This is the third defect of that shape on this item's CLI surface (`cfg node
// set` could not run on a node either), so the guard is deliberately not a unit
// test of the flag: it drives run() against real directories on disk and then
// looks at what is in them.

// TestCfgNodeSplitRunsAgainstARealDestination is the end-to-end guard.
func TestCfgNodeSplitRunsAgainstARealDestination(t *testing.T) {
	ctx := context.Background()
	gatewayPath := seededGatewayRoot(t)

	// Deliberately a directory that does NOT exist yet: the first hand-run of
	// this command died on a missing destination directory before it reached
	// anything interesting.
	nodePath := filepath.Join(t.TempDir(), "node", "config.yaml")

	out := captureStdout(t, func() {
		if err := run([]string{"--config", gatewayPath, "cfg", "node", "split",
			"--dest", nodePath, "--gateway", "wss://gw.example:8443"}); err != nil {
			t.Fatalf("cfg node split --dest: %v", err)
		}
	})

	// The node's half, opened as a node would open it.
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

	// And the gateway's half: it is still the gateway's, and the bodies are not
	// part of what it kept. `split` copies rather than moves — see
	// internal/config/store/split.go, deleting the agent rows is what would
	// switch the in-process path off — so the assertion is on the SPLIT's two
	// halves, which is what the command reports and what an operator reads.
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

	// The gateway root still loads as a gateway afterwards.
	gatewayCfg, gatewayStore := loadRoot(t, ctx, gatewayPath, config.Config{
		Role:  config.RoleGateway,
		OAuth: config.OAuthConfig{AppToken: "xapp-1", BotToken: "xoxb-1"},
	})
	defer gatewayStore.Close()
	if gatewayCfg.Access.AdminUser != "U1" {
		t.Errorf("the gateway lost its access block: %+v", gatewayCfg.Access)
	}
}

// TestCfgNodeSplitRefusesASecondGlobalConfigFlag pins the reason the flag is
// called --dest.
//
// `--config` reaches the whole command line by design, so a tool-scoped flag of
// the same name is unpassable. Silently retargeting the invocation is how this
// item shipped broken; saying so is the fix.
func TestCfgNodeSplitRefusesASecondGlobalConfigFlag(t *testing.T) {
	_, _, err := extractConfigFlag([]string{"--config", "/gw/config.yaml", "cfg", "node", "split", "--config", "/node/config.yaml"}, "/default")
	if err == nil {
		t.Fatal("a second --config was accepted; it silently becomes the target of the whole command")
	}
	if !strings.Contains(err.Error(), "--dest") {
		t.Errorf("the refusal does not name the flag to use instead: %v", err)
	}
}

// TestCfgNodeSplitValidatesBeforeItWrites keeps a refused argument from leaving
// a half-seeded root behind. The seed address used to be checked last, after the
// skeleton was written and the rows copied.
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

// seededGatewayRoot builds a combined installation on disk: a real bootstrap
// file plus the rows a pre-split install holds.
func seededGatewayRoot(t *testing.T) string {
	t.Helper()
	t.Setenv("SLACK_APP_TOKEN", "xapp-1")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-1")

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.Bootstrap(path); err != nil {
		t.Fatalf("seed a gateway root: %v", err)
	}
	ctx := context.Background()
	_, store, err := configstore.Bootstrap(ctx, path, true)
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

// loadRoot opens a configuration root the way the binary for that role would.
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

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
// The tool's result — what copied and what stayed — reaches an operator on
// stdout and nowhere else, so that is where it is asserted.
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
		// Restored even when fn calls t.Fatalf, which unwinds this goroutine.
		defer func() {
			os.Stdout = saved
			_ = write.Close()
		}()
		fn()
	}()
	return <-done
}
