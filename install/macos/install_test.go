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
	cmd := exec.Command("bash", "./install.sh", "--yes")
	cmd.Dir = "."
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
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

	agentsData, err := os.ReadFile(filepath.Join(home, ".config", "murtaugh", "agents.yaml"))
	if err != nil {
		t.Fatalf("read agents.yaml: %v", err)
	}
	agentsText := string(agentsData)
	if !strings.Contains(agentsText, "--acp") || !strings.Contains(agentsText, "--allow-indexing") {
		t.Fatalf("expected auggie ACP args in agents.yaml, got:\n%s", agentsText)
	}
	if strings.Contains(agentsText, "workspace-root") {
		t.Fatalf("agents.yaml unexpectedly set workspace root:\n%s", agentsText)
	}

	slackData, err := os.ReadFile(filepath.Join(home, ".config", "murtaugh", "slack.yaml"))
	if err != nil {
		t.Fatalf("read slack.yaml: %v", err)
	}
	if !strings.Contains(string(slackData), "default_agent: default") {
		t.Fatalf("slack.yaml missing default_agent:\n%s", slackData)
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
	if _, err := os.Stat(filepath.Join(home, ".config", "murtaugh", "slack.yaml")); !os.IsNotExist(err) {
		t.Fatalf("slack.yaml should not have been written, stat err=%v", err)
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
	if _, err := os.Stat(filepath.Join(home, ".config", "murtaugh", "slack.yaml")); !os.IsNotExist(err) {
		t.Fatalf("slack.yaml should not have been written, stat err=%v", err)
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
	if err := os.WriteFile(filepath.Join(configDir, "slack.yaml"), []byte("existing: true\n"), 0o644); err != nil {
		t.Fatalf("write existing slack.yaml: %v", err)
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
	content, err := os.ReadFile(filepath.Join(configDir, "slack.yaml"))
	if err != nil {
		t.Fatalf("read slack.yaml: %v", err)
	}
	if string(content) != "existing: true\n" {
		t.Fatalf("slack.yaml was overwritten unexpectedly: %s", content)
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
	if err := os.WriteFile(filepath.Join(configDir, "slack.yaml"), []byte("existing: true\n"), 0o644); err != nil {
		t.Fatalf("write existing slack.yaml: %v", err)
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
	content, err := os.ReadFile(filepath.Join(configDir, "slack.yaml"))
	if err != nil {
		t.Fatalf("read slack.yaml: %v", err)
	}
	if strings.Contains(string(content), "existing: true") {
		t.Fatalf("slack.yaml was not reconfigured as expected: %s", content)
	}
	if !strings.Contains(string(content), "app_token") {
		t.Fatalf("slack.yaml was not rewritten with new config: %s", content)
	}
}