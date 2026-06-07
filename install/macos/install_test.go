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

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeReleaseFixture(t *testing.T, dir string) string {
	t.Helper()
	asset := filepath.Join(dir, "murtaugh-v9.9.9-darwin-arm64")
	writeExecutable(t, asset, "#!/bin/sh\nif [ \"$1\" = \"--help\" ]; then exit 0; fi\nexit 0\n")
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
	if !strings.Contains(out, "Backed up "+settingsPath) {
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