package launchd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

type recorder struct {
	calls [][]string
	err   error
}

func (r *recorder) Run(ctx context.Context, name string, args ...string) error {
	full := append([]string{name}, args...)
	r.calls = append(r.calls, full)
	return r.err
}

func newDeps(t *testing.T, home string) (*Tool, *recorder, *recorder) {
	t.Helper()
	plutil := &recorder{}
	launchctl := &recorder{}
	return New(Deps{
		Home:      func() (string, error) { return home, nil },
		GOOS:      "darwin",
		Plutil:    plutil.Run,
		Launchctl: launchctl.Run,
	}), plutil, launchctl
}

func TestTool_Metadata(t *testing.T) {
	tl, _, _ := newDeps(t, t.TempDir())
	if tl.Name() != "setup.launchd" {
		t.Fatalf("Name() = %q, want setup.launchd", tl.Name())
	}
	schema := tl.InputSchema()
	if schema == nil {
		t.Fatal("InputSchema must not be nil")
	}
	if got := schema.Required; len(got) != 1 || got[0] != "binary_path" {
		t.Fatalf("required = %v, want [binary_path]", got)
	}
}

func TestInvoke_NonDarwinReturnsClearError(t *testing.T) {
	tl := New(Deps{
		Home:      func() (string, error) { return t.TempDir(), nil },
		GOOS:      "linux",
		Plutil:    func(context.Context, string, ...string) error { return nil },
		Launchctl: func(context.Context, string, ...string) error { return nil },
	})
	_, err := tl.Invoke(context.Background(), map[string]any{
		"binary_path": "/usr/local/bin/murtaugh",
	})
	if err == nil || !strings.Contains(err.Error(), "linux") {
		t.Fatalf("error = %v, want one mentioning linux", err)
	}
}

func TestInvoke_WritesPlistAndLogsDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("not relevant on windows")
	}
	home := t.TempDir()
	tl, plutil, launchctl := newDeps(t, home)

	res, err := tl.Invoke(context.Background(), map[string]any{
		"binary_path": "/usr/local/bin/murtaugh",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	r := res.(Result)
	wantPath := filepath.Join(home, "Library", "LaunchAgents", "dev.murtaugh.plist")
	if r.Path != wantPath {
		t.Fatalf("Path = %q, want %q", r.Path, wantPath)
	}
	body, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("plist missing: %v", err)
	}
	for _, want := range []string{"dev.murtaugh", "/usr/local/bin/murtaugh", "<string>slack</string>", "<string>gateway</string>"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("plist missing %q in:\n%s", want, body)
		}
	}
	logsDir := filepath.Join(home, "Library", "Logs", "murtaugh")
	if _, err := os.Stat(logsDir); err != nil {
		t.Fatalf("logs dir missing: %v", err)
	}
	if len(plutil.calls) != 1 {
		t.Fatalf("plutil called %d times, want 1", len(plutil.calls))
	}
	if len(launchctl.calls) != 0 {
		t.Fatalf("launchctl should not be called when load=false; got %v", launchctl.calls)
	}
	if r.Loaded {
		t.Fatal("Loaded must be false when load is not requested")
	}
}

func TestInvoke_LoadInvokesBootoutBootstrapThenKickstart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("not relevant on windows")
	}
	home := t.TempDir()
	tl, _, launchctl := newDeps(t, home)

	if _, err := tl.Invoke(context.Background(), map[string]any{
		"binary_path": "/usr/local/bin/murtaugh",
		"load":        true,
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(launchctl.calls) != 3 {
		t.Fatalf("launchctl calls = %v, want bootout+bootstrap+kickstart", launchctl.calls)
	}
	if launchctl.calls[0][1] != "bootout" || launchctl.calls[1][1] != "bootstrap" || launchctl.calls[2][1] != "kickstart" {
		t.Fatalf("calls order wrong: %v", launchctl.calls)
	}
	// kickstart must target the labeled service, not the bare domain, or
	// launchctl errors and the agent never spawns.
	last := launchctl.calls[2]
	if last[len(last)-1] != "gui/"+strconv.Itoa(os.Getuid())+"/dev.murtaugh" {
		t.Fatalf("kickstart target = %v, want gui/<uid>/dev.murtaugh", last)
	}
}

func TestInvoke_BootoutFailureIsToleratedBootstrapFailureSurfaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("not relevant on windows")
	}
	home := t.TempDir()
	plutil := &recorder{}
	launchctl := &recorder{}
	calls := 0
	launchctlFn := func(ctx context.Context, name string, args ...string) error {
		calls++
		full := append([]string{name}, args...)
		launchctl.calls = append(launchctl.calls, full)
		if calls == 1 {
			return errors.New("nothing to boot out")
		}
		return errors.New("bootstrap failed")
	}
	tl := New(Deps{
		Home:      func() (string, error) { return home, nil },
		GOOS:      "darwin",
		Plutil:    plutil.Run,
		Launchctl: launchctlFn,
	})
	_, err := tl.Invoke(context.Background(), map[string]any{
		"binary_path": "/usr/local/bin/murtaugh",
		"load":        true,
	})
	if err == nil || !strings.Contains(err.Error(), "bootstrap") {
		t.Fatalf("error = %v, want bootstrap failure surfaced", err)
	}
}

// TWO LAUNCHAGENTS (#200). A box running both halves of #170's split runs two
// daemons, and everything that identifies a job to launchd has to differ:
// otherwise the second write silently replaces the first, or the two interleave
// their logs and the troubleshoot bundle reports one machine's output as the
// other's.
func TestTheTwoRolesShareNothingThatIdentifiesAJob(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("plist writing is darwin-only")
	}
	home := t.TempDir()
	tl, _, _ := newDeps(t, home)

	gateway, err := tl.Invoke(context.Background(), map[string]any{
		"binary_path": "/opt/bin/murtaugh",
	})
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	node, err := tl.Invoke(context.Background(), map[string]any{
		"binary_path": "/opt/bin/murtaugh-runtime",
		"role":        "runtime",
	})
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}

	gw, rt := gateway.(Result), node.(Result)
	if gw.Label != "dev.murtaugh" {
		// Named in AGENTS.md, docs/operations.md, cli-help.md, the
		// murtaugh-setup skill, and every existing install's launchd registry.
		t.Fatalf("the gateway's label changed to %q; every runbook and every installed job names dev.murtaugh", gw.Label)
	}
	if rt.Label == gw.Label {
		t.Fatal("both roles wrote the same label, so the second job replaced the first")
	}
	if rt.Path == gw.Path {
		t.Fatal("both roles wrote the same plist file")
	}
	// Both must still be there: the second write must not have removed the
	// first, which a shared filename would have done invisibly.
	for _, path := range []string{gw.Path, rt.Path} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("plist %s is missing after writing both roles: %v", path, err)
		}
	}

	gwBody := readPlist(t, gw.Path)
	rtBody := readPlist(t, rt.Path)

	// Log files. The troubleshoot bundle and every runbook read
	// slack.{out,err}.log; a node appending to them interleaves with the
	// gateway's own output.
	if !strings.Contains(gwBody, "/slack.err.log") {
		t.Fatal("the gateway's error log is no longer slack.err.log")
	}
	if strings.Contains(rtBody, "slack.err.log") || !strings.Contains(rtBody, "/runtime.err.log") {
		t.Fatalf("the runtime shares the gateway's log files:\n%s", rtBody)
	}

	// Arguments. The gateway runs `slack gateway`; the runtime runs its own
	// binary with none, because it reads ~/.config/murtaugh/node by default and
	// a path in the plist would be a second place it is written down.
	if !strings.Contains(gwBody, "<string>slack</string>") {
		t.Fatal("the gateway plist no longer runs `slack gateway`")
	}
	if strings.Contains(rtBody, "<string>slack</string>") {
		t.Fatalf("the runtime plist runs a slack subcommand:\n%s", rtBody)
	}
	if !strings.Contains(rtBody, "<string>/opt/bin/murtaugh-runtime</string>") {
		t.Fatalf("the runtime plist does not name the runtime binary:\n%s", rtBody)
	}

	// KeepAlive on the node is load-bearing, not boilerplate: item 12's
	// zero-profile onboarding has the node write its configuration, answer the
	// gateway and then EXIT, relying on the supervisor to bring it back. Without
	// this a freshly onboarded node just dies.
	if !strings.Contains(rtBody, "<key>KeepAlive</key>\n    <true/>") {
		t.Fatalf("the runtime agent is not KeepAlive; a node that onboards and restarts would never come back:\n%s", rtBody)
	}
}

// Loading targets the role's own job. Kickstarting dev.murtaugh after writing
// the node's plist would restart the machine's Slack gateway instead.
func TestLoadingKickstartsTheRolesOwnLabel(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("plist writing is darwin-only")
	}
	tl, _, launchctl := newDeps(t, t.TempDir())
	if _, err := tl.Invoke(context.Background(), map[string]any{
		"binary_path": "/opt/bin/murtaugh-runtime",
		"role":        "runtime",
		"load":        true,
	}); err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var kickstarted string
	for _, call := range launchctl.calls {
		if len(call) >= 4 && call[1] == "kickstart" {
			kickstarted = call[len(call)-1]
		}
	}
	want := "gui/" + strconv.Itoa(os.Getuid()) + "/dev.murtaugh.runtime"
	if kickstarted != want {
		t.Fatalf("kickstarted %q, want %q — restarting the wrong label takes Slack down", kickstarted, want)
	}
}

// An unknown role is refused rather than defaulted. The two plists differ by
// which daemon they start, and quietly writing the gateway's over a typo is how
// a node install silently becomes a second gateway.
func TestAnUnknownRoleIsRefused(t *testing.T) {
	tl, _, _ := newDeps(t, t.TempDir())
	_, err := tl.Invoke(context.Background(), map[string]any{
		"binary_path": "/opt/bin/murtaugh",
		"role":        "runtme",
	})
	if err == nil {
		t.Fatal("a misspelled role was accepted")
	}
	if !strings.Contains(err.Error(), "runtme") {
		t.Fatalf("the refusal did not name the value: %v", err)
	}
}

func readPlist(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}
