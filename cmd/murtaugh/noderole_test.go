package main

import (
	"os"
	"path/filepath"
	"strings"
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

		// #200's installer runs these two against the node's own root. The rule
		// is one over every setup tool rather than a list of them, because the
		// rule that actually gets broken is the one that forgets a tool: a setup
		// command run without it seeds a config.yaml advertising
		// ${SLACK_APP_TOKEN} into the node's root before the tool it names has
		// done anything.
		"setup bootstrap --role runtime": {app.ModeCLI, []string{"setup", "bootstrap", "--role", "runtime"}, config.RoleNode},
		"setup launchd --role runtime":   {app.ModeCLI, []string{"setup", "launchd", "--role", "runtime", "--binary-path", "/x"}, config.RoleNode},
		"setup launchd --role=runtime":   {app.ModeCLI, []string{"setup", "launchd", "--role=runtime"}, config.RoleNode},
		"setup bootstrap --role gateway": {app.ModeCLI, []string{"setup", "bootstrap", "--role", "gateway"}, config.RoleCombined},
		"setup bootstrap":                {app.ModeCLI, []string{"setup", "bootstrap"}, config.RoleCombined},
		"setup launchd":                  {app.ModeCLI, []string{"setup", "launchd", "--binary-path", "/x"}, config.RoleCombined},
		"a --role elsewhere":             {app.ModeCLI, []string{"cfg", "agent", "create", "--role", "runtime"}, config.RoleCombined},
	} {
		if got := roleFor(tc.mode, tc.rest); got != tc.want {
			t.Errorf("%s: roleFor = %q, want %q", name, got, tc.want)
		}
	}
}

// The composition root seeds the configuration root it is pointed at BEFORE any
// tool runs, so the failure this guards is the one that happens before the
// operator's command does anything: `setup … --role runtime` against a node's
// directory creating a gateway config.yaml there.
//
// It is driven through run() rather than through config.BootstrapRole, because
// the bug was in the wiring and the function was always correct.
func TestASetupCommandForARuntimeNodeSeedsNoSlackCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// setup bootstrap is the seeding command, so the pre-seed and the tool must
	// agree — a disagreement is invisible, because bootstrap PRESERVES a
	// config.yaml that is already there.
	if err := run([]string{"--config", path, "setup", "bootstrap", "--role", "runtime"}); err != nil {
		t.Fatalf("setup bootstrap --role runtime: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the seeded config: %v", err)
	}
	if strings.Contains(string(body), "${SLACK_APP_TOKEN}") {
		t.Fatalf("a node's root was seeded from the gateway skeleton:\n%s", body)
	}

	// setup.launchd is the other command the installer runs against this root,
	// and it is the one that used to go wrong — it writes only into
	// ~/Library/LaunchAgents and looks like it touches no configuration at all.
	// It is asserted on the roleFor table above and DELIBERATELY not executed
	// here: the tool resolves the home directory from the RUNNING PROCESS's
	// environment, so driving it in process writes a LaunchAgent into the
	// developer's real ~/Library — pointing --config at a t.TempDir() does not
	// move it. That is the same hazard install_test.go's launchd_domain_is_ours
	// guard exists for, and a unit test is not the place to lean on it.
}
