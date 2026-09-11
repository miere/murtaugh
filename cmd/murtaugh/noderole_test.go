package main

import (
	"path/filepath"
	"testing"

	"github.com/miere/murtaugh/internal/app"
	"github.com/miere/murtaugh/internal/config"
)

// This drives run() end to end because the failure was in the composition root:
// the tool itself never ran.
func TestCfgNodeSetRunsAgainstANodeRoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.BootstrapNode(path); err != nil {
		t.Fatalf("seed a node root: %v", err)
	}

	if err := run([]string{"--config", path, "cfg", "node", "set",
		"--gateway", "wss://gateway.example.com:9443"}); err != nil {
		t.Fatalf("cfg node set on a node root: %v\n"+
			"a node's configuration has no oauth block by design, so an operator asked for one has nothing to do", err)
	}
	if err := run([]string{"--config", path, "cfg", "node", "show"}); err != nil {
		t.Fatalf("cfg node show on a node root: %v", err)
	}

	if err := run([]string{"--config", path, "cfg", "node", "set",
		"--gateway", "https://gateway.example.com"}); err == nil {
		t.Error("an https:// seed address was accepted")
	}
}

// `cfg node split` runs from the gateway's root, so it must keep the combined
// rules; so must everything else on this binary.
func TestRoleForNamesOnlyTheTwoCommandsThatAddressANodeRoot(t *testing.T) {
	for name, tc := range map[string]struct {
		mode app.Mode
		rest []string
		want config.Role
	}{
		"cfg node set":     {app.ModeCLI, []string{"cfg", "node", "set", "--gateway", "wss://gw:1"}, config.RoleNode},
		"cfg node show":    {app.ModeCLI, []string{"cfg", "node", "show"}, config.RoleNode},
		"cfg node split":   {app.ModeCLI, []string{"cfg", "node", "split"}, config.RoleCombined},
		"cfg chat set":     {app.ModeCLI, []string{"cfg", "chat", "set"}, config.RoleCombined},
		"cfg node":         {app.ModeCLI, []string{"cfg", "node"}, config.RoleCombined},
		"the daemon":       {app.ModeGateway, nil, config.RoleCombined},
		"mcp":              {app.ModeMCP, []string{"cfg", "node", "set"}, config.RoleCombined},
		"nothing at all":   {app.ModeCLI, nil, config.RoleCombined},
		"a tool named cfg": {app.ModeCLI, []string{"cfg"}, config.RoleCombined},
	} {
		if got := roleFor(tc.mode, tc.rest); got != tc.want {
			t.Errorf("%s: roleFor = %q, want %q", name, got, tc.want)
		}
	}
}
