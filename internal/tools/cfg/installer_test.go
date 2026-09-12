package cfg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/tools"
)

// Every test here writes into a temp LaunchAgents directory. A plist that
// landed in the operator's real ~/Library/LaunchAgents would sit next to a live
// daemon's, which is the whole reason --update-existing exists.
func launchdTestTool(t *testing.T, role config.Role, configPath string) (tools.Tool, string) {
	t.Helper()
	home := t.TempDir()
	agentsDir := filepath.Join(home, "Library", "LaunchAgents")
	deps := InstallerDeps{
		Role:            role,
		Home:            func() (string, error) { return home, nil },
		GOOS:            "darwin",
		Executable:      func() (string, error) { return "/opt/murtaugh/bin/" + binaryFor(role), nil },
		ConfigPath:      configPath,
		LaunchAgentsDir: agentsDir,
	}
	return find(t, InstallerTools(deps), "cfg.launchd"), agentsDir
}

func binaryFor(role config.Role) string {
	if role == config.RoleNode {
		return "murtaugh-runtime"
	}
	return "murtaugh-gateway"
}

func plistBody(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// The label is the join between a binary and the daemon launchd runs. Getting it
// wrong writes a plist that starts the wrong half, which is a finding this suite
// inherited from the shell installer it replaced.
func TestTheLabelNamesTheRoleAndDefaultsItsAlias(t *testing.T) {
	for role, want := range map[config.Role]string{
		config.RoleGateway: "murtaugh.gateway.default",
		config.RoleNode:    "murtaugh.node.default",
	} {
		tool, agentsDir := launchdTestTool(t, role, "/cfg/config.yaml")
		res, err := invoke(t, tool, nil)
		if err != nil {
			t.Fatalf("%s: cfg launchd: %v", role, err)
		}
		result := res.(interface{ String() string })
		if !strings.Contains(result.String(), want) {
			t.Errorf("%s: the confirmation does not name the label %q: %s", role, want, result.String())
		}
		path := filepath.Join(agentsDir, want+".plist")
		body := plistBody(t, path)
		if !strings.Contains(body, "<string>"+want+"</string>") {
			t.Errorf("%s: the plist carries no %s label:\n%s", role, want, body)
		}
		if bin := "/opt/murtaugh/bin/" + binaryFor(role); !strings.Contains(body, "<string>"+bin+"</string>") {
			t.Errorf("%s: the plist runs the wrong binary; want %s:\n%s", role, bin, body)
		}
	}
}

// Two nodes on one machine is what the alias exists for; today's constant label
// makes it impossible.
func TestTwoAliasesCoexistOnOneMachine(t *testing.T) {
	home := t.TempDir()
	agentsDir := filepath.Join(home, "Library", "LaunchAgents")
	newTool := func(configPath string) tools.Tool {
		return find(t, InstallerTools(InstallerDeps{
			Role:            config.RoleNode,
			Home:            func() (string, error) { return home, nil },
			GOOS:            "darwin",
			Executable:      func() (string, error) { return "/opt/murtaugh/bin/murtaugh-runtime", nil },
			ConfigPath:      configPath,
			LaunchAgentsDir: agentsDir,
		}), "cfg.launchd")
	}

	if _, err := invoke(t, newTool("/cfg/work/config.yaml"), map[string]any{"alias": "work"}); err != nil {
		t.Fatalf("first node: %v", err)
	}
	if _, err := invoke(t, newTool("/cfg/home/config.yaml"), map[string]any{"alias": "home"}); err != nil {
		t.Fatalf("second node: %v", err)
	}

	for alias, wantConfig := range map[string]string{"work": "/cfg/work/config.yaml", "home": "/cfg/home/config.yaml"} {
		body := plistBody(t, filepath.Join(agentsDir, "murtaugh.node."+alias+".plist"))
		if !strings.Contains(body, "<string>murtaugh.node."+alias+"</string>") {
			t.Errorf("the %s node's plist is not labelled for it:\n%s", alias, body)
		}
		if !strings.Contains(body, "<string>"+wantConfig+"</string>") {
			t.Errorf("the %s node's plist does not run against %s:\n%s", alias, wantConfig, body)
		}
	}
}

// Overwriting the plist that runs somebody's live daemon is not something to do
// on the way past. The old tool backed the file up and replaced it silently,
// leaving a backup nobody reads.
func TestAnExistingPlistIsRefusedWithoutUpdateExisting(t *testing.T) {
	tool, agentsDir := launchdTestTool(t, config.RoleGateway, "/cfg/config.yaml")
	if _, err := invoke(t, tool, nil); err != nil {
		t.Fatalf("first write: %v", err)
	}
	path := filepath.Join(agentsDir, "murtaugh.gateway.default.plist")
	if err := os.WriteFile(path, []byte("<!-- the live daemon's -->"), 0o644); err != nil {
		t.Fatalf("stand in for a live plist: %v", err)
	}

	_, err := invoke(t, tool, nil)
	if err == nil {
		t.Fatal("an existing plist was replaced silently")
	}
	if !strings.Contains(err.Error(), "--update-existing") {
		t.Errorf("the refusal does not name the flag that would allow it: %v", err)
	}
	if got := plistBody(t, path); got != "<!-- the live daemon's -->" {
		t.Errorf("the refused write changed the file anyway:\n%s", got)
	}

	if _, err := invoke(t, tool, map[string]any{"update_existing": true}); err != nil {
		t.Fatalf("--update-existing was refused too: %v", err)
	}
	if !strings.Contains(plistBody(t, path), "murtaugh.gateway.default") {
		t.Error("--update-existing did not replace the file")
	}
}

// The seed address decides which gateway a node comes back to after a reboot.
// It is written into the plist's arguments, and it is validated here rather than
// discovered inside the node's patient redial loop.
func TestTheNodesSeedAddressIsWrittenAndChecked(t *testing.T) {
	tool, agentsDir := launchdTestTool(t, config.RoleNode, "/cfg/node/config.yaml")

	if _, err := invoke(t, tool, map[string]any{"gateway": "https://gw.example"}); err == nil {
		t.Fatal("an https:// seed address was written into a LaunchAgent")
	}
	if _, err := os.Stat(filepath.Join(agentsDir, "murtaugh.node.default.plist")); !os.IsNotExist(err) {
		t.Errorf("the refused write left a plist behind: %v", err)
	}

	if _, err := invoke(t, tool, map[string]any{"gateway": "wss://gw.example:8443"}); err != nil {
		t.Fatalf("cfg launchd --gateway: %v", err)
	}
	body := plistBody(t, filepath.Join(agentsDir, "murtaugh.node.default.plist"))
	for _, want := range []string{"<string>-gateway</string>", "<string>wss://gw.example:8443</string>"} {
		if !strings.Contains(body, want) {
			t.Errorf("the plist does not carry %s:\n%s", want, body)
		}
	}
}

// A plist that carried the other role's flag would fail at launch, hours later,
// with launchd's own error rather than ours.
func TestEachBinaryRefusesTheOtherRolesFlags(t *testing.T) {
	node, _ := launchdTestTool(t, config.RoleNode, "/cfg/node/config.yaml")
	if _, err := invoke(t, node, map[string]any{"node_listen": "127.0.0.1:8787"}); err == nil {
		t.Error("the node binary accepted --node-listen, which only the gateway understands")
	}

	gateway, agentsDir := launchdTestTool(t, config.RoleGateway, "/cfg/config.yaml")
	if _, err := invoke(t, gateway, map[string]any{"gateway": "wss://gw.example:8443"}); err == nil {
		t.Error("the gateway binary accepted --gateway, which only a node dials")
	}
	if _, err := invoke(t, gateway, map[string]any{"node_listen": "127.0.0.1:8787"}); err != nil {
		t.Fatalf("cfg launchd -node-listen: %v", err)
	}
	body := plistBody(t, filepath.Join(agentsDir, "murtaugh.gateway.default.plist"))
	for _, want := range []string{"<string>-node-listen</string>", "<string>127.0.0.1:8787</string>"} {
		if !strings.Contains(body, want) {
			t.Errorf("the plist does not carry %s:\n%s", want, body)
		}
	}
}

// Writing is all it does. Loading is `launchctl bootstrap`, which the operator
// runs when they are ready for the daemon to start.
func TestLaunchdWritesThePlistAndNothingElse(t *testing.T) {
	tool, agentsDir := launchdTestTool(t, config.RoleGateway, "/cfg/config.yaml")
	res, err := invoke(t, tool, nil)
	if err != nil {
		t.Fatalf("cfg launchd: %v", err)
	}
	line := res.(interface{ String() string }).String()
	if !strings.Contains(line, "launchctl bootstrap") {
		t.Errorf("the confirmation does not say how to load it: %s", line)
	}
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		t.Fatalf("read the LaunchAgents dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("cfg launchd wrote %d files, want exactly the one plist", len(entries))
	}
}

func TestLaunchdIsRefusedOffMacOS(t *testing.T) {
	tool := find(t, InstallerTools(InstallerDeps{
		Role:       config.RoleGateway,
		Home:       func() (string, error) { return t.TempDir(), nil },
		GOOS:       "linux",
		Executable: func() (string, error) { return "/opt/murtaugh/bin/murtaugh-gateway", nil },
		ConfigPath: "/cfg/config.yaml",
	}), "cfg.launchd")

	_, err := invoke(t, tool, nil)
	if err == nil || !strings.Contains(err.Error(), "linux") {
		t.Fatalf("cfg launchd on linux gave %v, want a refusal naming the platform", err)
	}
}

// cfg migrate answers on a directory that has never been migrated, rather than
// reporting a missing schema stamp as a failure.
func TestCfgMigrateIsQuietOnAFreshDirectory(t *testing.T) {
	dir := t.TempDir()
	tool := find(t, InstallerTools(InstallerDeps{
		Role:       config.RoleNode,
		Home:       func() (string, error) { return dir, nil },
		GOOS:       "darwin",
		ConfigPath: filepath.Join(dir, "config.yaml"),
	}), "cfg.migrate")

	if _, err := tool.Invoke(context.Background(), nil); err != nil {
		t.Fatalf("cfg migrate on a fresh directory: %v", err)
	}
}
