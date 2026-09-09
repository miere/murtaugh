package main

import (
	"path/filepath"
	"testing"

	"github.com/miere/murtaugh/internal/app"
	"github.com/miere/murtaugh/internal/config"
)

// `murtaugh cfg node set --gateway …` is the documented way to point an
// installed node at its gateway. It is named in assets/cli-help.md, instructed
// inside assets/node-config.yaml, and offered as the remedy by
// murtaugh-runtime's own "no gateway address" error.
//
// Pointed at the node's own root it used to die on `oauth.app_token is
// required` — a credential a node must never hold and an operator therefore
// cannot supply. `node.gateway` is one of #198's headline additions and the only
// documented way to set it was unusable.

// TestCfgNodeSetRunsAgainstANodeRoot drives run() end to end against a real
// seeded node root, because the failure was in the composition root and not in
// the tool: the tool never ran.
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

	// The node's own rules still apply: this is a role, not a bypass.
	if err := run([]string{"--config", path, "cfg", "node", "set",
		"--gateway", "https://gateway.example.com"}); err == nil {
		t.Error("an https:// seed address was accepted")
	}
}

// TestRoleForNamesOnlyTheTwoCommandsThatAddressANodeRoot keeps the widening
// deliberate.
//
// `cfg node split` is the one `cfg node …` command that runs from the GATEWAY's
// root and writes the node's, which is why it already worked and why it must
// keep the combined rules. Everything else on this binary is a gateway or a
// combined install and must keep being asked for its Slack credentials.
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
