package macos

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// --role gateway|runtime|both (#200). Two daemons with distinct labels and
// separate configuration roots, so a crash-looping node does not take Slack
// down with it — and a runtime node that needs exactly two things to attach is
// told, by the installer, what and where they are.
//
// Everything here goes through runInstaller, so it inherits the two guards that
// make these tests safe to have: `-short` skips them entirely, and
// launchd_domain_is_ours refuses to touch the real launchd session when HOME is
// a temp dir. They run for real on the dedicated macos runner in CI.

// writeRoleReleaseFixture is writeReleaseFixture plus the split binaries, which
// is what a release publishes once #200's workflow change lands. The runtime
// asset is the CLI binary under another name: what is under test is that the
// installer FETCHES and PLACES it, not what it does when run.
func writeRoleReleaseFixture(t *testing.T, dir string) string {
	t.Helper()
	built := buildMurtaughBinary(t)
	assets := []map[string]any{}
	for _, name := range []string{"murtaugh", "murtaugh-gateway", "murtaugh-runtime"} {
		asset := filepath.Join(dir, name+"-v9.9.9-darwin-arm64")
		copyFile(t, built, asset, 0o755)
		assets = append(assets, map[string]any{
			"name":                 name + "-v9.9.9-darwin-arm64",
			"browser_download_url": "file://" + asset,
		})
	}
	data, err := json.Marshal(map[string]any{"tag_name": "v9.9.9", "assets": assets})
	if err != nil {
		t.Fatalf("marshal release: %v", err)
	}
	path := filepath.Join(dir, "release.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write release fixture: %v", err)
	}
	return path
}

func runRoleInstaller(t *testing.T, home, role string, extra ...string) string {
	t.Helper()
	binDir := filepath.Join(home, ".local", "bin")
	env := append([]string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"MURTAUGH_RELEASE_JSON_PATH=" + writeRoleReleaseFixture(t, t.TempDir()),
		"MURTAUGH_INSTALL_ARCH=arm64",
		"MURTAUGH_ROLE=" + role,
	}, extra...)
	out, err := runInstaller(t, env)
	if err != nil {
		t.Fatalf("installer --role %s failed: %v\n%s", role, err, out)
	}
	return out
}

// --role runtime installs a node and nothing Slack-facing. The gateway's own
// root must not be seeded on a machine that serves no Slack: a config.yaml
// advertising ${SLACK_APP_TOKEN} on a laptop is an invitation to fill in
// credentials #170 says that machine must never hold.
func TestInstallerRoleRuntimeInstallsANodeAndNoGateway(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	home := t.TempDir()
	out := runRoleInstaller(t, home, "runtime")

	nodeDir := filepath.Join(home, ".config", "murtaugh", "node")
	nodeYAML := filepath.Join(nodeDir, "config.yaml")
	if _, err := os.Stat(nodeYAML); err != nil {
		t.Fatalf("the node's configuration root was not seeded: %v", err)
	}
	// The node skeleton is defined by what it does NOT contain. Matched on the
	// substitution and on the block's own line — the node asset explains in a
	// COMMENT that it has no `oauth:` block, and a bare substring check would
	// pass on the gateway skeleton and fail on the right one.
	body := readFileString(t, nodeYAML)
	if strings.Contains(body, "${SLACK_APP_TOKEN}") || strings.Contains(body, "\noauth:") {
		t.Fatalf("the node was seeded from the gateway skeleton; it advertises Slack credentials it must never hold:\n%s", body)
	}
	if env := readFileString(t, filepath.Join(nodeDir, ".env")); strings.Contains(env, "SLACK_APP_TOKEN=") {
		t.Fatalf("the node's .env invites the workspace's Slack tokens:\n%s", env)
	}
	// Its own root, not a second file in the gateway's directory: config/migrate
	// restores every top-level file in the directory it runs in, so a shared one
	// would let a failed migration in either role restore over the other's
	// credentials.
	if _, err := os.Stat(filepath.Join(home, ".config", "murtaugh", "config.yaml")); err == nil {
		t.Fatal("--role runtime seeded a gateway configuration too")
	}

	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "murtaugh-runtime")); err != nil {
		t.Fatalf("murtaugh-runtime was not installed, so its LaunchAgent points at nothing: %v", err)
	}

	rtPlist := filepath.Join(home, "Library", "LaunchAgents", "dev.murtaugh.runtime.plist")
	if _, err := os.Stat(rtPlist); err != nil {
		t.Fatalf("the runtime LaunchAgent was not written: %v", err)
	}
	// What the installer RENDERED, rather than a path a unit test supplied to
	// renderPlist itself. The node's daemon is murtaugh-runtime; handed the CLI
	// it would exec `murtaugh` with no arguments, print usage, exit, and be
	// respawned forever by KeepAlive into runtime.err.log.
	rendered := readFileString(t, rtPlist)
	if !strings.Contains(rendered, "<string>"+filepath.Join(home, ".local", "bin", "murtaugh-runtime")+"</string>") {
		t.Errorf("the node's LaunchAgent does not run the murtaugh-runtime binary:\n%s", rendered)
	}
	if strings.Contains(rendered, "<string>"+filepath.Join(home, ".local", "bin", "murtaugh")+"</string>") {
		t.Errorf("the node's LaunchAgent runs the CLI, which prints usage and exits:\n%s", rendered)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", "dev.murtaugh.plist")); err == nil {
		t.Fatal("--role runtime wrote the Slack gateway's LaunchAgent")
	}

	// A node needs exactly two things to attach, and both are printed.
	if !strings.Contains(out, "node token mint") {
		t.Errorf("the installer did not say how to get a node token:\n%s", out)
	}
	if !strings.Contains(out, filepath.Join(nodeDir, "node-token")) {
		t.Errorf("the installer did not say where the node token goes:\n%s", out)
	}
	if !strings.Contains(out, "cfg node set --gateway") {
		t.Errorf("no --gateway was given, so the installer had to say how to set one:\n%s", out)
	}
	if !strings.Contains(out, "kickstart -k gui/$(id -u)/dev.murtaugh.runtime") {
		t.Errorf("the installer did not say how to start the node:\n%s", out)
	}
}

// The seed address is recorded in the node's own store, not printed as homework,
// when the operator supplied one. `cfg node set` is one of the few commands that
// loads at RoleNode; run at RoleCombined it would die on "oauth.app_token is
// required" mid-script, asking for a credential a node must never hold.
func TestInstallerRecordsTheGatewaySeedAddress(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	home := t.TempDir()
	out := runRoleInstaller(t, home, "runtime", "MURTAUGH_GATEWAY=wss://gw.example.test:8443")

	if !strings.Contains(out, "wss://gw.example.test:8443") {
		t.Errorf("the installer did not report the seed address it recorded:\n%s", out)
	}
	if strings.Contains(out, "cfg node set --gateway wss://your-gateway") {
		t.Errorf("the installer told the operator to set an address it had already set:\n%s", out)
	}
	if got := nodeConfigDump(t, home); !strings.Contains(got, "wss://gw.example.test:8443") {
		t.Fatalf("the seed address is not in the node's store:\n%s", got)
	}
}

// --role both is the loopback pair on one machine: two LaunchAgents, two
// configuration roots, and a seed address it can work out for itself.
func TestInstallerRoleBothWritesTwoIndependentDaemons(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	home := t.TempDir()
	out := runRoleInstaller(t, home, "both")

	agents := filepath.Join(home, "Library", "LaunchAgents")
	for _, label := range []string{"dev.murtaugh", "dev.murtaugh.runtime"} {
		if _, err := os.Stat(filepath.Join(agents, label+".plist")); err != nil {
			t.Fatalf("--role both did not write %s: %v", label, err)
		}
	}
	// Separate log files. A node appending to slack.err.log interleaves with the
	// gateway's own output, and that file is what every runbook and the
	// troubleshoot bundle read.
	gw := readFileString(t, filepath.Join(agents, "dev.murtaugh.plist"))
	rt := readFileString(t, filepath.Join(agents, "dev.murtaugh.runtime.plist"))
	if !strings.Contains(gw, "slack.err.log") {
		t.Errorf("the gateway's log file changed:\n%s", gw)
	}
	if strings.Contains(rt, "slack.err.log") {
		t.Errorf("the node shares the gateway's log file:\n%s", rt)
	}

	// Separate roots.
	for _, path := range []string{
		filepath.Join(home, ".config", "murtaugh", "config.yaml"),
		filepath.Join(home, ".config", "murtaugh", "node", "config.yaml"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("--role both did not seed %s: %v", path, err)
		}
	}

	// The pair is on one machine, so the seed is knowable and is recorded rather
	// than left as homework. Whatever it picks becomes the de facto default —
	// there is no port constant anywhere in the tree.
	if got := nodeConfigDump(t, home); !strings.Contains(got, "ws://127.0.0.1:8787") {
		t.Fatalf("--role both did not record a loopback seed address:\n%s", got)
	}
	if !strings.Contains(out, "direct message") {
		t.Errorf("--role both dropped the gateway hand-off:\n%s", out)
	}
}

// Both daemons are left STOPPED, which is the installer's character and not an
// accident of ordering. Murtaugh can do nothing until real tokens are in .env,
// and a node can do nothing until it has a credential.
func TestInstallerRoleBothLeavesBothDaemonsStopped(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	home := t.TempDir()
	out := runRoleInstaller(t, home, "both")
	for _, label := range []string{"dev.murtaugh", "dev.murtaugh.runtime"} {
		if !strings.Contains(out, "kickstart -k gui/$(id -u)/"+label) {
			t.Errorf("the installer did not say how to start %s:\n%s", label, out)
		}
	}
	if strings.Contains(out, "Restarted LaunchAgent") {
		t.Errorf("the installer started a daemon:\n%s", out)
	}
}

// Reinstalling must be safe, and the token is where it is least safe: the file
// is written with O_EXCL and refuses to be overwritten, and mint writes it
// BEFORE storing the record — so a mint-on-every-run installer would abort the
// second install under `set -euo pipefail` and leave an orphan credential
// registered on the gateway. The installer therefore never mints; it reports.
func TestReinstallingARuntimeNodeKeepsItsToken(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	home := t.TempDir()
	runRoleInstaller(t, home, "runtime", "MURTAUGH_GATEWAY=wss://gw.example.test:8443")

	tokenPath := filepath.Join(home, ".config", "murtaugh", "node", "node-token")
	if _, err := os.Stat(tokenPath); err == nil {
		t.Fatal("the installer minted a node token; it cannot know the Slack user id one needs")
	}
	if err := os.WriteFile(tokenPath, []byte("mrtg_node_abcdef.secret\n"), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	out := runRoleInstaller(t, home, "runtime", "MURTAUGH_GATEWAY=wss://gw.example.test:8443")
	body := readFileString(t, tokenPath)
	if !strings.Contains(body, "mrtg_node_abcdef.secret") {
		t.Fatalf("a reinstall replaced the node's credential: %q", body)
	}
	if !strings.Contains(out, "one is already at") {
		t.Errorf("the installer did not report the existing credential:\n%s", out)
	}
	// And the mode is untouched: the node refuses to read a token looser than
	// 0600, so an installer that chmod'ed or copied it would break the attach.
	info, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the node token's mode is %o; the node refuses anything looser than 0600", perm)
	}
}

// A typo must not silently install the wrong thing. parse_args dies on an
// unknown argument, and a role has to be treated the same way.
func TestAnUnknownRoleIsRefused(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	if testing.Short() {
		t.Skip("skipping installer test in -short mode: it runs the real macOS installer")
	}
	cmd := exec.Command("bash", "./install.sh", "--role", "runtme")
	cmd.Stdin = nil
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("a misspelled role was accepted:\n%s", out)
	}
	if !strings.Contains(string(out), "runtme") {
		t.Errorf("the refusal did not name the value:\n%s", out)
	}
}

// The default is still the gateway, byte for byte. A published curl|bash line
// that passes no --role must keep getting exactly what it got yesterday, and
// this is the assertion that catches a default flipped by a later edit.
func TestTheDefaultRoleIsStillTheGateway(t *testing.T) {
	script := readFileString(t, "install.sh")
	if !strings.Contains(script, `ROLE="gateway"`) {
		t.Error("the installer's default role is no longer gateway")
	}
}

// nodeConfigDump reads the node root's own configuration back through the CLI,
// at RoleNode — which is the only role that can load a root with no oauth block.
func nodeConfigDump(t *testing.T, home string) string {
	t.Helper()
	bin := filepath.Join(home, ".local", "bin", "murtaugh")
	nodeYAML := filepath.Join(home, ".config", "murtaugh", "node", "config.yaml")
	cmd := exec.Command(bin, "--config", nodeYAML, "cfg", "node", "show")
	env := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "HOME=") || strings.HasPrefix(e, "XDG_STATE_HOME=") {
			continue
		}
		env = append(env, e)
	}
	cmd.Env = append(env, "HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cfg node show failed: %v\n%s", err, out)
	}
	return string(out)
}

// The JOIN, not the two ends of it. #200's plist bugs are all one token wide and
// all invisible from either side.
//
// install.sh renders both LaunchAgents through one helper and restarts through
// another, and both take the distinguishing value as an argument: the gateway's
// plist runs the CLI under dev.murtaugh, the node's runs murtaugh-runtime under
// dev.murtaugh.runtime. `$installed_bin` and `$runtime_bin` sat adjacent on the
// same line, and the labels differ by a suffix.
//
// Nothing caught a swap. The launchd unit test that checks the plist names
// `/opt/bin/murtaugh-runtime` SUPPLIES that path itself; across the whole
// install/macos suite the only content assertion made against
// dev.murtaugh.runtime.plist is that it does not mention slack.err.log; and the
// restart helper returns before launchctl under any test's temp HOME, so both
// existing assertions negative-match a line that is never emitted anyway.
//
// So the two pairings are functions, and this reads them: the helpers are
// replaced with recorders, which is also what keeps this test off launchctl and
// out of ~/Library entirely.
func recordLaunchAgentPlan(t *testing.T, call string) string {
	t.Helper()
	script := `
set -euo pipefail
source ./install.sh
write_launch_agent() { printf 'write_launch_agent cli=%s role=%s target=%s config=%s\n' "$1" "$2" "$3" "$4"; }
restart_launch_agent_if_needed() { printf 'restart label=%s\n' "$1"; }
` + call
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sourcing install.sh failed: %v\n%s", err, out)
	}
	return string(out)
}

// The node's daemon must run murtaugh-runtime. Handed the CLI instead it would
// exec `murtaugh` with no arguments, which prints usage and exits — and
// KeepAlive, which item 12's onboarding restart depends on, would respawn it
// forever into runtime.err.log.
func TestTheRuntimeLaunchAgentRunsTheRuntimeBinaryUnderItsOwnLabel(t *testing.T) {
	out := recordLaunchAgentPlan(t,
		`install_runtime_launch_agent /opt/bin/murtaugh /opt/bin/murtaugh-runtime /home/.config/murtaugh/node/config.yaml`)

	if !strings.Contains(out, "target=/opt/bin/murtaugh-runtime") {
		t.Errorf("the node's LaunchAgent does not run murtaugh-runtime:\n%s", out)
	}
	if strings.Contains(out, "target=/opt/bin/murtaugh\n") || strings.Contains(out, "target=/opt/bin/murtaugh ") {
		t.Errorf("the node's LaunchAgent runs the CLI; it would print usage, exit, and be respawned forever by KeepAlive:\n%s", out)
	}
	if !strings.Contains(out, "role=runtime") {
		t.Errorf("the node's plist was not written for the runtime role:\n%s", out)
	}
	if !strings.Contains(out, "config=/home/.config/murtaugh/node/config.yaml") {
		t.Errorf("the node's LaunchAgent was pointed at the wrong configuration root:\n%s", out)
	}
	// The label decides which daemon a node-only update bounces. dev.murtaugh is
	// the live Slack gateway.
	if !strings.Contains(out, "restart label=dev.murtaugh.runtime") {
		t.Errorf("a node-only update does not restart the node's own agent:\n%s", out)
	}
	if strings.Contains(out, "restart label=dev.murtaugh\n") {
		t.Errorf("a node-only update bounces the LIVE Slack gateway:\n%s", out)
	}
}

// And the other half, unchanged: the gateway is still the CLI under
// dev.murtaugh. Its label, arguments and log filenames are named in AGENTS.md,
// docs/operations.md, cli-help.md and every existing install's launchd registry,
// so changing one is a migration rather than an improvement.
func TestTheGatewayLaunchAgentStillRunsTheCLIUnderDevMurtaugh(t *testing.T) {
	out := recordLaunchAgentPlan(t,
		`install_gateway_launch_agent /opt/bin/murtaugh /home/.config/murtaugh/config.yaml`)

	if !strings.Contains(out, "role=gateway") || !strings.Contains(out, "target=/opt/bin/murtaugh") {
		t.Errorf("the gateway's LaunchAgent no longer runs the CLI:\n%s", out)
	}
	if !strings.Contains(out, "restart label=dev.murtaugh\n") {
		t.Errorf("the gateway's label changed; every existing install's launchd registry names dev.murtaugh:\n%s", out)
	}
}

// A reinstall must not evict a seed the node already holds.
//
// `cfg node set --gateway` ASSIGNS the list — `cfg.Gateway = v`, and the tool's
// own schema says "repeatable; replaces the list" — which is right for a `set`
// command and wrong for an installer that runs unattended on every update. A
// node is deliberately allowed several addresses because a gateway is often
// known by more than one name and item 11's failover walks them in order, so a
// re-run passing one --gateway used to silently delete the redundancy that
// exists for the case where one of those names stops resolving.
//
// This drives the real helper against a real node root and the real CLI. It runs
// no installer, writes no plist and never reaches launchctl.
func TestReinstallingWithASecondGatewayKeepsTheFirst(t *testing.T) {
	bin := buildMurtaughBinary(t)
	home := t.TempDir()
	nodeDir := filepath.Join(home, ".config", "murtaugh", "node")
	nodeYAML := filepath.Join(nodeDir, "config.yaml")
	// seed_node_root runs first in the installer and is what creates the
	// directory; this helper is only ever called after it.
	if err := os.MkdirAll(nodeDir, 0o700); err != nil {
		t.Fatalf("mkdir node root: %v", err)
	}

	seed := func(address string) string {
		t.Helper()
		cmd := exec.Command("bash", "-c",
			"set -euo pipefail\nsource ./install.sh\nwrite_node_seed_address \"$1\" \"$2\" \"$3\"\nexisting_node_seeds \"$1\" \"$2\"",
			"bash", bin, nodeYAML, address)
		cmd.Dir = "."
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("write_node_seed_address %s: %v\n%s", address, err, out)
		}
		return string(out)
	}

	seed("wss://gw-a.example:8443")
	out := seed("wss://gw-b.example:8443")
	for _, want := range []string{"wss://gw-a.example:8443", "wss://gw-b.example:8443"} {
		if !strings.Contains(out, want) {
			t.Errorf("a reinstall with a second --gateway left the node holding %s but not %s;\n"+
				"the address it lost is the one item 11's failover exists to fall back to:\n%s", out, want, out)
		}
	}
	// Dial ORDER is stable: what worked yesterday is still tried first.
	if strings.Index(out, "wss://gw-a.example:8443") > strings.Index(out, "wss://gw-b.example:8443") {
		t.Errorf("the seed order was reshuffled; a node walks them in order:\n%s", out)
	}

	// And an update that passes the address the node already has does not grow a
	// duplicate on every run.
	out = seed("wss://gw-b.example:8443")
	if n := strings.Count(out, "wss://gw-b.example:8443"); n != 1 {
		t.Errorf("re-passing a known seed recorded it %d times:\n%s", n, out)
	}
}
