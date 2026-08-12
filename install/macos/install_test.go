package macos

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// builtBinaryOnce caches the murtaugh binary built for the release fixture.
// The bash installer now delegates config writes to `murtaugh setup ...`, so
// the asset must be the real binary rather than an exit-0 shell stub.
var (
	builtBinaryOnce sync.Once
	builtBinaryPath string
	builtBinaryErr  error
)

func buildMurtaughBinary(t *testing.T) string {
	t.Helper()
	builtBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "murtaugh-build-")
		if err != nil {
			builtBinaryErr = err
			return
		}
		bin := filepath.Join(dir, "murtaugh")
		cmd := exec.Command("go", "build", "-ldflags=-X main.version=v9.9.9", "-o", bin, "../../cmd/murtaugh")
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			builtBinaryErr = err
			return
		}
		builtBinaryPath = bin
	})
	if builtBinaryErr != nil {
		t.Fatalf("build murtaugh: %v", builtBinaryErr)
	}
	return builtBinaryPath
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func copyFile(t *testing.T, src, dst string, perm os.FileMode) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dst, err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		t.Fatalf("copy %s -> %s: %v", src, dst, err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close %s: %v", dst, err)
	}
}

func writeReleaseFixture(t *testing.T, dir string) string {
	t.Helper()
	asset := filepath.Join(dir, "murtaugh-v9.9.9-darwin-arm64")
	copyFile(t, buildMurtaughBinary(t), asset, 0o755)
	release := map[string]any{
		"tag_name": "v9.9.9",
		"assets": []map[string]any{{
			"name":                 "murtaugh-v9.9.9-darwin-arm64",
			"browser_download_url": "file://" + asset,
		}},
	}
	data, err := json.Marshal(release)
	if err != nil {
		t.Fatalf("marshal release: %v", err)
	}
	path := filepath.Join(dir, "release.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write release fixture: %v", err)
	}
	return path
}

func runInstaller(t *testing.T, env []string) (string, error) {
	t.Helper()
	// These tests drive the REAL installer, which writes a dev.murtaugh plist and
	// (absent its sandbox guard) would reach into gui/$(id -u). That is a live
	// service on any machine hosting a gateway, so `-short` must offer a way to
	// run the rest of the suite without it. Gating here covers every caller.
	if testing.Short() {
		t.Skip("skipping installer test in -short mode: it runs the real macOS installer")
	}
	cmd := exec.Command("bash", "./install.sh", "--yes")
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// configDump runs the installed binary's `cfg show` (against the same HOME the
// installer used, so the SQLite config store path resolves identically) and
// returns the JSON dump. Agents, chat, and access now live in the store rather
// than in YAML siblings, so the installer tests assert on this dump.
func configDump(t *testing.T, home string) string {
	t.Helper()
	bin := filepath.Join(home, ".local", "bin", "murtaugh")
	gw := filepath.Join(home, ".config", "murtaugh", "config.yaml")
	cmd := exec.Command(bin, "--config", gw, "cfg", "show")
	// Pass a SINGLE HOME (and drop XDG_STATE_HOME) so the binary resolves the
	// same ~/.local/state/murtaugh/config.db the installer wrote to. Appending a
	// second HOME would leave the process's real HOME first, and macOS getenv
	// returns the first match.
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
		t.Fatalf("cfg show failed: %v\n%s", err, out)
	}
	// `cfg show` pretty-prints the JSON; flatten whitespace so compact substring
	// assertions match regardless of indentation. Config values carry no internal
	// spaces, so this is lossless for our checks.
	return strings.Join(strings.Fields(string(out)), "")
}

func TestInstallerConfiguresAuggieAndBacksUpMCPSettings(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	home := t.TempDir()
	binDir := filepath.Join(home, "bin")
	releaseJSON := writeReleaseFixture(t, t.TempDir())
	writeExecutable(t, filepath.Join(binDir, "auggie"), "#!/bin/sh\nexit 0\n")

	settingsPath := filepath.Join(home, ".augment", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatalf("mkdir settings dir: %v", err)
	}
	if err := os.WriteFile(settingsPath, []byte(`{"theme":"dark"}`), 0o644); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	out, err := runInstaller(t, []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"MURTAUGH_RELEASE_JSON_PATH=" + releaseJSON,
		"MURTAUGH_INSTALL_ARCH=arm64",
		"MURTAUGH_SLACK_APP_TOKEN=xapp-test-token",
		"MURTAUGH_SLACK_BOT_TOKEN=xoxb-test-token",
		"MURTAUGH_ADMIN_USER=@admin",
		"MURTAUGH_CHAT_AGENT=auggie",
		"MURTAUGH_ENABLE_LAUNCH_AGENT=yes",
		"MURTAUGH_LOAD_LAUNCH_AGENT=no",
		"MURTAUGH_MCP_CLIENT=auggie",
	})
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}

	installedBin := filepath.Join(home, ".local", "bin", "murtaugh")
	if _, err := os.Stat(installedBin); err != nil {
		t.Fatalf("installed binary missing: %v", err)
	}
	realInstalledBin, err := filepath.EvalSymlinks(installedBin)
	if err != nil {
		t.Fatalf("EvalSymlinks(installedBin): %v", err)
	}

	// The auggie agent (an ACP backend) and the chat routing now live in the
	// config store, not in YAML siblings.
	dump := configDump(t, home)
	if !strings.Contains(dump, "--acp") || !strings.Contains(dump, "--allow-indexing") {
		t.Fatalf("expected auggie ACP args in the config store, got:\n%s", dump)
	}
	if strings.Contains(dump, "workspace-root") {
		t.Fatalf("config unexpectedly set workspace root:\n%s", dump)
	}
	if !strings.Contains(dump, `"agent":"default"`) {
		t.Fatalf("chat.defaults.agent not stored:\n%s", dump)
	}

	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", "dev.murtaugh.plist")); err != nil {
		t.Fatalf("LaunchAgent missing: %v", err)
	}

	updatedSettings, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read updated settings: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(updatedSettings, &parsed); err != nil {
		t.Fatalf("parse settings json: %v", err)
	}
	mcpServers, ok := parsed["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing from settings: %v", parsed)
	}
	murtaugh, ok := mcpServers["murtaugh"].(map[string]any)
	if !ok {
		t.Fatalf("murtaugh MCP entry missing: %v", mcpServers)
	}
	if got := murtaugh["command"]; got != realInstalledBin {
		t.Fatalf("command = %v, want %s", got, realInstalledBin)
	}
	// setup.mcp-register reports the backup path as part of its Result
	// string ("(backup: <path>)"). The old bash logger prefix went away
	// with the install.sh rewrite; we now assert on the tool's notice.
	if !strings.Contains(out, "backup: "+settingsPath+".bak.") {
		t.Fatalf("expected backup notice in output, got:\n%s", out)
	}
	matches, err := filepath.Glob(settingsPath + ".bak.*")
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected one settings backup, got %v err=%v", matches, err)
	}
}

func TestInstallerConfiguresNativeAgent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	home := t.TempDir()
	binDir := filepath.Join(home, "bin")
	releaseJSON := writeReleaseFixture(t, t.TempDir())

	out, err := runInstaller(t, []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"MURTAUGH_RELEASE_JSON_PATH=" + releaseJSON,
		"MURTAUGH_INSTALL_ARCH=arm64",
		"MURTAUGH_SLACK_APP_TOKEN=xapp-test-token",
		"MURTAUGH_SLACK_BOT_TOKEN=xoxb-test-token",
		"MURTAUGH_ADMIN_USER=@admin",
		"MURTAUGH_CHAT_AGENT=native",
		"MURTAUGH_NATIVE_PROVIDER=gemini",
		"MURTAUGH_NATIVE_MODEL=gemini-2.5-pro",
		"MURTAUGH_NATIVE_API_KEY=test-gemini-key-123",
		"MURTAUGH_ENABLE_LAUNCH_AGENT=no",
		"MURTAUGH_MCP_CLIENT=skip",
	})
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}

	configDir := filepath.Join(home, ".config", "murtaugh")

	// The native agent lives in the config store now.
	dump := configDump(t, home)
	for _, want := range []string{`"native"`, `"provider":"gemini"`, `"model":"gemini-2.5-pro"`, `"api_key_env":"GEMINI_API_KEY"`} {
		if !strings.Contains(dump, want) {
			t.Fatalf("config store missing %q:\n%s", want, dump)
		}
	}
	// A native agent carries no process command / acp args (the defaults singleton
	// has its own acp: block, so we check for the agent-level markers instead).
	if strings.Contains(dump, `"--acp"`) || strings.Contains(dump, `"claude_code"`) {
		t.Fatalf("native agent must not carry a process backend block:\n%s", dump)
	}

	// The API key must land in .env, never in YAML.
	envText := readFileString(t, filepath.Join(configDir, ".env"))
	if !strings.Contains(envText, "GEMINI_API_KEY=test-gemini-key-123") {
		t.Fatalf(".env missing the provider key:\n%s", envText)
	}
	slackText := readFileString(t, filepath.Join(configDir, "config.yaml"))
	if strings.Contains(slackText, "test-gemini-key-123") || strings.Contains(slackText, "xapp-test-token") {
		t.Fatalf("a secret leaked into config.yaml:\n%s", slackText)
	}
	// chat.defaults.agent now lives in the config store, not config.yaml.
	if !strings.Contains(dump, `"agent":"default"`) {
		t.Fatalf("chat.defaults.agent not stored:\n%s", dump)
	}
	// Slack tokens also went to .env.
	if !strings.Contains(envText, "SLACK_APP_TOKEN=xapp-test-token") {
		t.Fatalf(".env missing Slack token:\n%s", envText)
	}
	// The key must not be echoed in installer output.
	if strings.Contains(out, "test-gemini-key-123") {
		t.Fatalf("installer output leaked the API key:\n%s", out)
	}
}

func TestInstallerFailsBeforeWritingConfigWhenAgentMissing(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	home := t.TempDir()
	releaseJSON := writeReleaseFixture(t, t.TempDir())

	out, err := runInstaller(t, []string{
		"HOME=" + home,
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"MURTAUGH_RELEASE_JSON_PATH=" + releaseJSON,
		"MURTAUGH_INSTALL_ARCH=arm64",
		"MURTAUGH_SLACK_APP_TOKEN=xapp-test-token",
		"MURTAUGH_SLACK_BOT_TOKEN=xoxb-test-token",
		"MURTAUGH_ADMIN_USER=@admin",
		"MURTAUGH_CHAT_AGENT=goose",
		"MURTAUGH_ENABLE_LAUNCH_AGENT=no",
		"MURTAUGH_MCP_CLIENT=skip",
	})
	if err == nil {
		t.Fatalf("installer succeeded unexpectedly:\n%s", out)
	}
	if !strings.Contains(out, "goose is not installed") {
		t.Fatalf("expected missing goose error, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "murtaugh", "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("config.yaml should not have been written, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "murtaugh", "agents.yaml")); !os.IsNotExist(err) {
		t.Fatalf("agents.yaml should not have been written, stat err=%v", err)
	}
}

func TestInstallerSkipsUpdateWhenAlreadyCurrent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	releaseJSON := writeReleaseFixture(t, t.TempDir())

	// Install a fake murtaugh that reports v9.9.9
	writeExecutable(t, filepath.Join(binDir, "murtaugh"), "#!/bin/sh\nif [ \"$1\" = \"version\" ]; then echo 'v9.9.9'; exit 0; fi\nexit 0\n")

	out, err := runInstaller(t, []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"MURTAUGH_RELEASE_JSON_PATH=" + releaseJSON,
		"MURTAUGH_INSTALL_ARCH=arm64",
		"MURTAUGH_SLACK_APP_TOKEN=xapp-test-token",
		"MURTAUGH_SLACK_BOT_TOKEN=xoxb-test-token",
		"MURTAUGH_ADMIN_USER=@admin",
		"MURTAUGH_CHAT_AGENT=skip",
		"MURTAUGH_ENABLE_LAUNCH_AGENT=no",
		"MURTAUGH_MCP_CLIENT=skip",
	})
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Already running v9.9.9") {
		t.Fatalf("expected skip update message, got:\n%s", out)
	}
	if strings.Contains(out, "Updated Murtaugh") {
		t.Fatalf("should not have updated binary, got:\n%s", out)
	}
}

func TestInstallerForcesUpdateWhenCurrent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	releaseJSON := writeReleaseFixture(t, t.TempDir())

	// Install a fake murtaugh that reports v9.9.9
	writeExecutable(t, filepath.Join(binDir, "murtaugh"), "#!/bin/sh\nif [ \"$1\" = \"version\" ]; then echo 'v9.9.9'; exit 0; fi\nexit 0\n")

	out, err := runInstaller(t, []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"MURTAUGH_RELEASE_JSON_PATH=" + releaseJSON,
		"MURTAUGH_INSTALL_ARCH=arm64",
		"MURTAUGH_SLACK_APP_TOKEN=xapp-test-token",
		"MURTAUGH_SLACK_BOT_TOKEN=xoxb-test-token",
		"MURTAUGH_ADMIN_USER=@admin",
		"MURTAUGH_CHAT_AGENT=skip",
		"MURTAUGH_ENABLE_LAUNCH_AGENT=no",
		"MURTAUGH_MCP_CLIENT=skip",
		"MURTAUGH_FORCE_INSTALL=yes",
	})
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Updated Murtaugh from v9.9.9 to v9.9.9") {
		t.Fatalf("expected forced update message, got:\n%s", out)
	}
}

func TestInstallerSkipConfigUpdatesBinaryOnly(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	releaseJSON := writeReleaseFixture(t, t.TempDir())

	out, err := runInstaller(t, []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"MURTAUGH_RELEASE_JSON_PATH=" + releaseJSON,
		"MURTAUGH_INSTALL_ARCH=arm64",
		"MURTAUGH_SKIP_CONFIG=yes",
	})
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Binary updated; config untouched") {
		t.Fatalf("expected skip config message, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "murtaugh", "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("config.yaml should not have been written, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "murtaugh", "agents.yaml")); !os.IsNotExist(err) {
		t.Fatalf("agents.yaml should not have been written, stat err=%v", err)
	}
}

func TestInstallerPreservesConfigByDefault(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	releaseJSON := writeReleaseFixture(t, t.TempDir())

	// Pre-seed existing config
	configDir := filepath.Join(home, ".config", "murtaugh")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("existing: true\n"), 0o644); err != nil {
		t.Fatalf("write existing config.yaml: %v", err)
	}

	out, err := runInstaller(t, []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"MURTAUGH_RELEASE_JSON_PATH=" + releaseJSON,
		"MURTAUGH_INSTALL_ARCH=arm64",
		"MURTAUGH_CHAT_AGENT=skip",
		"MURTAUGH_ENABLE_LAUNCH_AGENT=no",
		"MURTAUGH_MCP_CLIENT=skip",
	})
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Preserving Slack and agent configs by default") {
		t.Fatalf("expected preserve config message, got:\n%s", out)
	}
	content, err := os.ReadFile(filepath.Join(configDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if string(content) != "existing: true\n" {
		t.Fatalf("config.yaml was overwritten unexpectedly: %s", content)
	}
}

func TestInstallerReconfiguresWhenRequested(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	releaseJSON := writeReleaseFixture(t, t.TempDir())
	writeExecutable(t, filepath.Join(binDir, "auggie"), "#!/bin/sh\nexit 0\n")

	// Pre-seed existing config
	configDir := filepath.Join(home, ".config", "murtaugh")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("existing: true\n"), 0o644); err != nil {
		t.Fatalf("write existing config.yaml: %v", err)
	}

	out, err := runInstaller(t, []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"MURTAUGH_RELEASE_JSON_PATH=" + releaseJSON,
		"MURTAUGH_INSTALL_ARCH=arm64",
		"MURTAUGH_SLACK_APP_TOKEN=xapp-test-token",
		"MURTAUGH_SLACK_BOT_TOKEN=xoxb-test-token",
		"MURTAUGH_ADMIN_USER=@admin",
		"MURTAUGH_CHAT_AGENT=auggie",
		"MURTAUGH_ENABLE_LAUNCH_AGENT=no",
		"MURTAUGH_MCP_CLIENT=skip",
		"MURTAUGH_RECONFIGURE=yes",
	})
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "Preserving Slack and agent configs by default") {
		t.Fatalf("should not have preserved config when --reconfigure, got:\n%s", out)
	}
	content, err := os.ReadFile(filepath.Join(configDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	if strings.Contains(string(content), "existing: true") {
		t.Fatalf("config.yaml was not reconfigured as expected: %s", content)
	}
	if !strings.Contains(string(content), "app_token") {
		t.Fatalf("config.yaml was not rewritten with new config: %s", content)
	}
}

// TestInstallerHasNoPythonDependency guards against reintroducing inline
// python heredocs into install.sh. The orchestrator rewrite explicitly
// promised to be python-free; a stray python3 invocation has burned us
// before on user machines that interpret JSON differently or where
// system python is missing.
func TestInstallerHasNoPythonDependency(t *testing.T) {
	data, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(line, "python3 ") || strings.Contains(line, "python ") {
			t.Fatalf("install.sh must not invoke python; found: %q", line)
		}
	}
}

// TestPromptChoiceLoopsOnInvalidInteractiveInput guards the UX regression
// where a typo at a multi-option prompt (e.g. answering "yes" to
// "Configure Murtaugh as an MCP server in a client?") aborted the entire
// install mid-flight. The interactive path must instead render the
// available options, warn on bad input, and re-prompt until valid.
// Scripted callers (env var / --yes) still fail fast.
func TestPromptChoiceLoopsOnInvalidInteractiveInput(t *testing.T) {
	scriptPath, err := filepath.Abs("install.sh")
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	harness := `set -uo pipefail
ASSUME_YES=0; SKIP_CONFIG=0; RECONFIGURE=0; FORCE_INSTALL=0
DRY_RUN=0; LOCAL_BUILD=0; TARGET_VERSION=""
source "` + scriptPath + `"
prompt_choice MURTAUGH_MCP_CLIENT "Configure Murtaugh as an MCP server in a client?" skip skip opencode auggie goose
`
	cmd := exec.Command("bash", "-c", harness)
	cmd.Stdin = strings.NewReader("yes\nskip\n")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("prompt_choice harness failed: %v\nstdout=%q\nstderr=%s", err, stdout, stderr.String())
	}
	if got := strings.TrimSpace(string(stdout)); got != "skip" {
		t.Fatalf("expected final choice 'skip', got %q (stderr=%s)", got, stderr.String())
	}
	errOut := stderr.String()
	if !strings.Contains(errOut, "Invalid choice: yes") {
		t.Fatalf("expected re-prompt warning naming the bad input; stderr=%s", errOut)
	}
	for _, opt := range []string{"skip", "opencode", "auggie", "goose"} {
		if !strings.Contains(errOut, opt) {
			t.Fatalf("expected available option %q listed for the user; stderr=%s", opt, errOut)
		}
	}
}

// TestPromptChoiceRejectsInvalidEnvVar pins the scripted-caller contract:
// when MURTAUGH_MCP_CLIENT (or any env-driven choice) holds an invalid
// value, the installer must abort with a clear error rather than block
// waiting for stdin. Loop-on-invalid is interactive-only.
func TestPromptChoiceRejectsInvalidEnvVar(t *testing.T) {
	scriptPath, err := filepath.Abs("install.sh")
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	harness := `set -uo pipefail
ASSUME_YES=0; SKIP_CONFIG=0; RECONFIGURE=0; FORCE_INSTALL=0
DRY_RUN=0; LOCAL_BUILD=0; TARGET_VERSION=""
export MURTAUGH_MCP_CLIENT=yes
source "` + scriptPath + `"
prompt_choice MURTAUGH_MCP_CLIENT "Configure Murtaugh as an MCP server in a client?" skip skip opencode auggie goose
`
	cmd := exec.Command("bash", "-c", harness)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected scripted invalid value to abort; got success with output:\n%s", out)
	}
	if !strings.Contains(string(out), "invalid value 'yes' for MURTAUGH_MCP_CLIENT") {
		t.Fatalf("expected explicit invalid-value diagnostic; got:\n%s", out)
	}
}

// TestInstallerRejectsLegacyBinaryWithoutSetup guards the regression where
// install.sh installs a binary that does not yet support `murtaugh setup`
// and then crashes mid-install with `unknown command: setup`. The new
// capability check should bail out before any setup call is attempted and
// suggest --local-build (because we're running from a checkout).
func TestInstallerRejectsLegacyBinaryWithoutSetup(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	home := t.TempDir()
	fixtureDir := t.TempDir()
	// Stub binary mimics v0.0.1: `version` works, `setup` is unknown.
	stub := filepath.Join(fixtureDir, "murtaugh-v0.0.1-darwin-arm64")
	stubScript := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  version) echo v0.0.1 ;;\n" +
		"  setup) echo 'murtaugh: unknown command: setup' >&2; exit 2 ;;\n" +
		"  *) echo unknown >&2; exit 1 ;;\n" +
		"esac\n"
	if err := os.WriteFile(stub, []byte(stubScript), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	release := map[string]any{
		"tag_name": "v0.0.1",
		"assets": []map[string]any{{
			"name":                 "murtaugh-v0.0.1-darwin-arm64",
			"browser_download_url": "file://" + stub,
		}},
	}
	data, _ := json.Marshal(release)
	releasePath := filepath.Join(fixtureDir, "release.json")
	if err := os.WriteFile(releasePath, data, 0o644); err != nil {
		t.Fatalf("write release fixture: %v", err)
	}

	out, err := runInstaller(t, []string{
		"HOME=" + home,
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"MURTAUGH_RELEASE_JSON_PATH=" + releasePath,
		"MURTAUGH_INSTALL_DIR=" + filepath.Join(home, "bin"),
		"MURTAUGH_INSTALL_ARCH=arm64",
	})
	if err == nil {
		t.Fatalf("installer should refuse a binary without setup support; output:\n%s", out)
	}
	if strings.Contains(out, "unknown command: setup") {
		t.Fatalf("installer should fail before invoking setup, but reached it; output:\n%s", out)
	}
	if !strings.Contains(out, "does not support 'setup'") {
		t.Fatalf("installer should explain the missing-setup failure; got:\n%s", out)
	}
	if !strings.Contains(out, "--local-build") {
		t.Fatalf("installer should suggest --local-build when run from a checkout; got:\n%s", out)
	}
}

// TestInstallerFailsCleanlyWhenReleaseMissing covers the failure mode the
// user hit when running the installer against a repo without a published
// release: release_json returned a 404, the prior implementation crashed
// with `parsed[0]: unbound variable`, and the user saw a python stacktrace.
// The new code must produce a single, human-readable error instead.
func TestInstallerFailsCleanlyWhenReleaseMissing(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	home := t.TempDir()
	// Point at a nonexistent file so release_json fails the same way curl
	// would fail with a 404, without actually hitting the network.
	out, err := runInstaller(t, []string{
		"HOME=" + home,
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"MURTAUGH_RELEASE_JSON_PATH=/nonexistent/release.json",
		"MURTAUGH_INSTALL_ARCH=arm64",
		"MURTAUGH_SKIP_CONFIG=yes",
	})
	if err == nil {
		t.Fatalf("installer should have failed when release metadata is missing, got:\n%s", out)
	}
	if strings.Contains(out, "Traceback") || strings.Contains(out, "python") {
		t.Fatalf("installer should not surface python errors, got:\n%s", out)
	}
	if strings.Contains(out, "unbound variable") {
		t.Fatalf("installer should handle missing release without bash unbound errors, got:\n%s", out)
	}
	if !strings.Contains(out, "could not fetch release metadata") && !strings.Contains(out, "release metadata") {
		t.Fatalf("installer should print a clear error about the missing release, got:\n%s", out)
	}
}

// launchdSandboxEnv is the common installer environment for the launchd-safety
// tests: a native agent (no external binary needed) and a LaunchAgent enabled so
// the plist exists and restart_launch_agent_if_needed reaches its guards.
func launchdSandboxEnv(t *testing.T, home string) []string {
	t.Helper()
	binDir := filepath.Join(home, "bin")
	return []string{
		"HOME=" + home,
		"PATH=" + binDir + ":/usr/bin:/bin:/usr/sbin:/sbin",
		"MURTAUGH_RELEASE_JSON_PATH=" + writeReleaseFixture(t, t.TempDir()),
		"MURTAUGH_INSTALL_ARCH=arm64",
		"MURTAUGH_SLACK_APP_TOKEN=xapp-test-token",
		"MURTAUGH_SLACK_BOT_TOKEN=xoxb-test-token",
		"MURTAUGH_ADMIN_USER=@admin",
		"MURTAUGH_CHAT_AGENT=native",
		"MURTAUGH_NATIVE_PROVIDER=gemini",
		"MURTAUGH_NATIVE_MODEL=gemini-2.5-pro",
		"MURTAUGH_NATIVE_API_KEY=test-gemini-key-123",
		"MURTAUGH_ENABLE_LAUNCH_AGENT=yes",
		"MURTAUGH_MCP_CLIENT=skip",
	}
}

// TestInstallerHonoursLoadLaunchAgentNo pins the answer to
// MURTAUGH_LOAD_LAUNCH_AGENT. write_launch_agent always honoured it, but
// restart_launch_agent_if_needed used to run unconditionally afterwards and
// bootout+bootstrap+kickstart the agent anyway — silently overriding the "no".
func TestInstallerHonoursLoadLaunchAgentNo(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	home := t.TempDir()
	out, err := runInstaller(t, append(launchdSandboxEnv(t, home), "MURTAUGH_LOAD_LAUNCH_AGENT=no"))
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}

	// The plist is still written — only the loading was declined.
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", "dev.murtaugh.plist")); err != nil {
		t.Fatalf("LaunchAgent plist should still be written: %v", err)
	}
	if !strings.Contains(out, "MURTAUGH_LOAD_LAUNCH_AGENT=no") {
		t.Fatalf("installer should report that it skipped the restart, got:\n%s", out)
	}
	if strings.Contains(out, "Restarted LaunchAgent dev.murtaugh") {
		t.Fatalf("installer restarted the LaunchAgent despite MURTAUGH_LOAD_LAUNCH_AGENT=no, got:\n%s", out)
	}
}

// TestInstallerRefusesLaunchdFromSandboxedHome is the regression guard for the
// incident that motivated these tests: this suite runs the installer with
// HOME=t.TempDir(), but every launchctl call targets gui/$(id -u) — the real
// login session. The installer therefore booted out the developer's running
// gateway and bootstrapped the temp plist in its place, leaving a daemon running
// from a since-deleted temp binary that could never reconnect to Slack.
//
// Loading is requested here (the default), so the refusal must come from the
// HOME-vs-login-home check rather than from the load choice.
func TestInstallerRefusesLaunchdFromSandboxedHome(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	home := t.TempDir()
	out, err := runInstaller(t, append(launchdSandboxEnv(t, home), "MURTAUGH_LOAD_LAUNCH_AGENT=yes"))
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}

	if !strings.Contains(out, "is not the login home") {
		t.Fatalf("installer should refuse to touch launchd from a sandboxed HOME, got:\n%s", out)
	}
	if strings.Contains(out, "Restarted LaunchAgent dev.murtaugh") {
		t.Fatalf("installer touched the live launchd session from a sandboxed HOME, got:\n%s", out)
	}
}
