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

// TestInstallerSeedsConfigSkeletonWithoutSecrets covers the whole of what the
// installer now writes: a config.yaml and a .env the operator fills in.
//
// The point of the rewrite is that the installer stops asking. So the assertion
// is not only that the skeleton exists but that it contains PLACEHOLDERS —
// anything else would mean the installer had prompted for, or invented, a
// credential.
func TestInstallerSeedsConfigSkeletonWithoutSecrets(t *testing.T) {
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
	})
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}

	configDir := filepath.Join(home, ".config", "murtaugh")
	yaml := readFileString(t, filepath.Join(configDir, "config.yaml"))
	env := readFileString(t, filepath.Join(configDir, ".env"))

	// The tokens are referenced, never held.
	if !strings.Contains(yaml, "${SLACK_APP_TOKEN}") {
		t.Errorf("config.yaml does not reference the Slack token:\n%s", yaml)
	}
	if !strings.Contains(env, "SLACK_APP_TOKEN=") {
		t.Errorf(".env has no slot for the Slack token:\n%s", env)
	}
	// The alternative backends must be discoverable in the file itself: the
	// operator is told to "review the storage backend" and never sees a prompt.
	for _, want := range []string{"firestore", "postgres", "sqlite"} {
		if !strings.Contains(yaml, want) {
			t.Errorf("config.yaml does not mention the %s backend:\n%s", want, yaml)
		}
	}
}

// TestInstallerLeavesTheDaemonStopped is the deliberate behaviour change.
//
// Murtaugh cannot do anything until real tokens are in .env, so an agent
// started here only crash-loops behind the instructions the operator is still
// reading. The plist is written; starting it is their step.
func TestInstallerLeavesTheDaemonStopped(t *testing.T) {
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
	})
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}

	plist := filepath.Join(home, "Library", "LaunchAgents", "dev.murtaugh.plist")
	if _, err := os.Stat(plist); err != nil {
		t.Fatalf("the LaunchAgent plist was not written: %v", err)
	}
	// The hand-off has to tell them how to start it, or the install dead-ends.
	if !strings.Contains(out, "launchctl kickstart") {
		t.Errorf("the installer does not say how to start Murtaugh:\n%s", out)
	}
	if !strings.Contains(out, "direct message") {
		t.Errorf("the installer does not hand off to Slack:\n%s", out)
	}
}

// TestInstallerAsksNothing pins the property the rewrite exists for. It runs
// with no configuration environment variables at all and with stdin closed: a
// surviving prompt would hang or fail rather than quietly defaulting.
func TestInstallerAsksNothing(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	if testing.Short() {
		t.Skip("skipping installer test in -short mode: it runs the real macOS installer")
	}
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	releaseJSON := writeReleaseFixture(t, t.TempDir())

	cmd := exec.Command("bash", "./install.sh")
	cmd.Dir = "."
	cmd.Stdin = nil // no terminal, no answers
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PATH="+binDir+":/usr/bin:/bin:/usr/sbin:/sbin",
		"MURTAUGH_RELEASE_JSON_PATH="+releaseJSON,
		"MURTAUGH_INSTALL_ARCH=arm64",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("installer needed input it should not have asked for: %v\n%s", err, out)
	}
	// Note the absence of --yes: it is accepted but no longer means anything.
	if _, err := os.Stat(filepath.Join(home, ".config", "murtaugh", "config.yaml")); err != nil {
		t.Errorf("no config skeleton was written: %v", err)
	}
}

// TestInstallerPreservesExistingConfig keeps a re-run from clobbering a
// configured install — the update path is the common one.
func TestInstallerPreservesExistingConfig(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("installer is macOS-only")
	}
	home := t.TempDir()
	binDir := filepath.Join(home, ".local", "bin")
	releaseJSON := writeReleaseFixture(t, t.TempDir())

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
	})
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	if got := readFileString(t, filepath.Join(configDir, "config.yaml")); got != "existing: true\n" {
		t.Fatalf("a re-run overwrote an existing config: %s", got)
	}
	if !strings.Contains(out, "leaving it alone") {
		t.Errorf("the installer did not say it preserved the config:\n%s", out)
	}
}

// TestInstallerIsNonInteractiveBySource guards the property structurally as
// well as behaviourally: a reintroduced prompt helper would pass the runtime
// tests on a machine with a terminal and hang on a CI runner without one.
func TestInstallerIsNonInteractiveBySource(t *testing.T) {
	script := readFileString(t, "install.sh")
	for _, banned := range []string{"prompt_choice", "prompt_required"} {
		if strings.Contains(script, banned) {
			t.Errorf("install.sh contains %q; the installer must ask nothing", banned)
		}
	}
	// A `read` is only interactive when it reads from stdin. Splitting a string
	// with a herestring is not a prompt, and banning the word outright would
	// forbid the version comparator for no reason.
	for i, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "read ") || strings.Contains(trimmed, "<<<") {
			continue
		}
		t.Errorf("install.sh line %d reads from stdin (%q); the installer must ask nothing", i+1, trimmed)
	}
}
