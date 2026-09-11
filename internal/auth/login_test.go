package auth

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A sign-in that ran outside the agent's environment once reported success
// while the agent still found no credential.
func TestCommandSpecLayersTheAgentEnvironment(t *testing.T) {
	t.Setenv("CLOUDSDK_CONFIG", "/home/op/.config/gcloud")
	t.Setenv("MURTAUGH_TEST_UNTOUCHED", "keep-me")

	profile, err := Resolve("gcloud", "", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	spec, cleanup, err := commandSpec(profile, []string{"CLOUDSDK_CONFIG=/srv/work/.gcloud"})
	if err != nil {
		t.Fatalf("commandSpec: %v", err)
	}
	defer cleanup()

	if !slices.Contains(spec.Env, "CLOUDSDK_CONFIG=/srv/work/.gcloud") {
		t.Error("the agent's CLOUDSDK_CONFIG did not reach the spawned command")
	}
	if slices.Contains(spec.Env, "CLOUDSDK_CONFIG=/home/op/.config/gcloud") {
		t.Error("the .env value survived alongside the agent's; the sign-in would be ambiguous")
	}
	if !slices.Contains(spec.Env, "MURTAUGH_TEST_UNTOUCHED=keep-me") {
		t.Error("the inherited environment was dropped instead of layered")
	}
}

// Pinning a snapshot of the environment would freeze it at the wrong moment for
// a caller with nothing to override.
func TestCommandSpecInheritsWithoutAnAgentEnvironment(t *testing.T) {
	profile, err := Resolve("gcloud", "", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	spec, cleanup, err := commandSpec(profile, nil)
	if err != nil {
		t.Fatalf("commandSpec: %v", err)
	}
	defer cleanup()
	if spec.Env != nil {
		t.Errorf("Spec.Env = %v, want nil so the command inherits", spec.Env)
	}
}

// `claude auth login` would open its consent page on the machine it runs on,
// and its opener looks `open` up on PATH, so that is where the guard goes.
func TestCommandSpecGuardsTheBrowserForClaudeCode(t *testing.T) {
	profile, err := Resolve("claude-code", "", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	spec, cleanup, err := commandSpec(profile, nil)
	if err != nil {
		t.Fatalf("commandSpec: %v", err)
	}
	defer cleanup()

	guardDir := ""
	for _, entry := range spec.Env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			guardDir, _, _ = strings.Cut(value, string(os.PathListSeparator))
		}
	}
	if guardDir == "" {
		t.Fatal("no PATH override was applied; the CLI would find the real launcher")
	}
	for _, name := range []string{"open", "xdg-open"} {
		info, err := os.Stat(filepath.Join(guardDir, name))
		if err != nil {
			t.Fatalf("stat guard %s: %v", name, err)
		}
		if info.Mode().Perm()&0o100 == 0 {
			t.Errorf("guard %s is not executable, so PATH lookup would skip past it", name)
		}
	}
	if !slices.ContainsFunc(spec.Env, func(entry string) bool {
		value, ok := strings.CutPrefix(entry, "PATH=")
		return ok && strings.Contains(value, string(os.PathListSeparator))
	}) {
		t.Error("PATH was replaced rather than prepended to")
	}
}

// A guard left behind per sign-in would litter the machine with binaries that
// shadow the real launchers.
func TestCommandSpecCleanupRemovesTheGuard(t *testing.T) {
	profile, err := Resolve("claude-code", "", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	spec, cleanup, err := commandSpec(profile, nil)
	if err != nil {
		t.Fatalf("commandSpec: %v", err)
	}

	guardDir := ""
	for _, entry := range spec.Env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			guardDir, _, _ = strings.Cut(value, string(os.PathListSeparator))
		}
	}
	cleanup()
	if _, err := os.Stat(guardDir); !os.IsNotExist(err) {
		t.Errorf("guard directory %s survived cleanup (err=%v)", guardDir, err)
	}
}

// An agent that could put its own PATH back could put a browser window back on
// the machine's desktop.
func TestCommandSpecGuardOutranksTheAgentEnvironment(t *testing.T) {
	profile, err := Resolve("claude-code", "", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	spec, cleanup, err := commandSpec(profile, []string{"PATH=/agent/only"})
	if err != nil {
		t.Fatalf("commandSpec: %v", err)
	}
	defer cleanup()

	for _, entry := range spec.Env {
		value, ok := strings.CutPrefix(entry, "PATH=")
		if !ok {
			continue
		}
		if value == "/agent/only" {
			t.Fatal("the agent's PATH replaced the guard; the browser would open on the host")
		}
		if !strings.Contains(value, "/agent/only") {
			t.Errorf("PATH = %q, want the agent's entry preserved behind the guard", value)
		}
	}
}

func TestStartLoginHandsBackTheLinkAndTakesTheCode(t *testing.T) {
	p, err := Custom(`echo "Go to https://example.com/auth?x=1 to sign in"; read code; [ "$code" = GOOD ]`, true)
	if err != nil {
		t.Fatalf("Custom: %v", err)
	}
	login, url, err := StartLogin(context.Background(), p, nil, 10*time.Second)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	defer login.Stop()
	if url != "https://example.com/auth?x=1" {
		t.Fatalf("url = %q", url)
	}
	if err := login.SendCode("GOOD"); err != nil {
		t.Fatalf("SendCode: %v", err)
	}
	select {
	case <-login.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("the sign-in never finished after its code")
	}
	if ok, detail := login.Result(); !ok {
		t.Fatalf("a correct code failed: %s", detail)
	}
}

// The agent's environment wins over the node's own, so the credential lands
// where the agent that asked will look for it.
func TestStartLoginRunsInTheAgentsEnvironmentOverTheNodes(t *testing.T) {
	t.Setenv("MURTAUGH_LOGIN_TEST", "node")
	t.Setenv("MURTAUGH_LOGIN_INHERITED", "kept")
	p, err := Custom(`echo "https://example.com/auth?v=$MURTAUGH_LOGIN_TEST&i=$MURTAUGH_LOGIN_INHERITED"`, false)
	if err != nil {
		t.Fatalf("Custom: %v", err)
	}
	login, url, err := StartLogin(context.Background(), p, []string{"MURTAUGH_LOGIN_TEST=agent"}, 10*time.Second)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	defer login.Stop()
	if url != "https://example.com/auth?v=agent&i=kept" {
		t.Fatalf("the sign-in ran with %q; want the agent's value over the node's, and the rest inherited", url)
	}
}

func TestStartLoginFailsWithoutALink(t *testing.T) {
	p, err := Custom(`echo "nothing to open here"`, false)
	if err != nil {
		t.Fatalf("Custom: %v", err)
	}
	if _, _, err := StartLogin(context.Background(), p, nil, 10*time.Second); err == nil || !strings.Contains(err.Error(), "without offering a sign-in link") {
		t.Fatalf("a sign-in with no link started: %v", err)
	}
}

func TestStopKillsTheSignIn(t *testing.T) {
	p, err := Custom(`echo "https://example.com/auth"; exec sleep 30`, false)
	if err != nil {
		t.Fatalf("Custom: %v", err)
	}
	login, _, err := StartLogin(context.Background(), p, nil, 10*time.Second)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	login.Stop()
	select {
	case <-login.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("the sign-in survived Stop")
	}
	login.Stop()
}
