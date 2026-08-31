package onboarding

import (
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

// claudeCodeDraft is a form filled in for the self-authenticating backend,
// which is the one the guards apply to.
func claudeCodeDraft() Draft {
	d := NewDraft()
	d.Step = StepOptions
	d.Kind = KindClaudeCode
	d.Model = "opus"
	d.WorkDir = "/srv/work"
	return d
}

// TestGeneralProfileGetsToolsIsTheRegression. An empty allowlist is not a
// permissive default: toolset.Resolve selects only what the list names, so a
// profile built without one comes up able to talk and unable to act, with
// nothing in the config or the logs that looks like an error.
func TestGeneralProfileGetsTools(t *testing.T) {
	out, err := Build(completeDraft(), "/cfg", "U01ADMIN")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(out.Default.Tools) == 0 {
		t.Fatal("the general profile was built with no tools; it would answer without being able to do anything")
	}
	for _, want := range []string{"ask", "auth", "attach", "slack", "files", "terminal"} {
		if !slices.Contains(out.Default.Tools, want) {
			t.Errorf("default tools %v are missing %q", out.Default.Tools, want)
		}
	}
}

// TestTweakerGetsEveryToolFamily is the other half of the same bug. The
// administrator's profile exists to finish the install from Slack, and a family
// withheld from it is a job it cannot do.
func TestTweakerGetsEveryToolFamily(t *testing.T) {
	out, err := Build(completeDraft(), "/cfg", "U01ADMIN")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, family := range ToolFamilies {
		if !slices.Contains(out.Tweaker.Tools, family.Name) {
			t.Errorf("the tweaker is missing the %q family", family.Name)
		}
	}
}

// TestOperatorToolChoiceWins checks the picker is actually read, including the
// answer that looks like no answer: clearing every box.
func TestOperatorToolChoiceWins(t *testing.T) {
	d := completeDraft()
	d.Step = StepOptions
	d.Tools = []string{"slack"}
	d.ToolsChosen = true

	out, err := Build(d, "/cfg", "U01ADMIN")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !slices.Equal(out.Default.Tools, []string{"slack"}) {
		t.Errorf("default tools = %v, want just the chosen one", out.Default.Tools)
	}

	d.Tools = nil
	out, err = Build(d, "/cfg", "U01ADMIN")
	if err != nil {
		t.Fatalf("Build with an emptied picker: %v", err)
	}
	if len(out.Default.Tools) != 0 {
		t.Errorf("clearing the picker gave %v, want no tools: an operator's explicit answer was overwritten with the defaults", out.Default.Tools)
	}
}

// TestSandboxFollowsTheCheckbox pins both directions. Onboarding sandboxed
// every Claude Code agent before this step existed, so an unticked box has to
// actually turn it off rather than fall back to the old behaviour.
func TestSandboxFollowsTheCheckbox(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skipf("no sandbox mode is available on %s", runtime.GOOS)
	}
	d := claudeCodeDraft()
	d.Sandboxed = true
	out, err := Build(d, "/cfg", "U01ADMIN")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if out.Default.Sandbox.Mode != config.SandboxModeSeatbelt {
		t.Errorf("sandbox mode = %q, want %q", out.Default.Sandbox.Mode, config.SandboxModeSeatbelt)
	}

	d.Sandboxed = false
	out, err = Build(d, "/cfg", "U01ADMIN")
	if err != nil {
		t.Fatalf("Build unsandboxed: %v", err)
	}
	if out.Default.Sandbox.Mode != config.SandboxModeOff {
		t.Errorf("sandbox mode = %q with the box unticked, want %q", out.Default.Sandbox.Mode, config.SandboxModeOff)
	}
}

// TestSandboxIsRefusedWhereItCannotBeApplied. Silently building an unconfined
// profile for an operator who asked for confinement is worse than either honest
// outcome: they would believe the agent was boxed in.
func TestSandboxIsRefusedWhereItCannotBeApplied(t *testing.T) {
	d := completeDraft() // a native backend: no process to confine
	d.Step = StepOptions
	d.Sandboxed = true
	if _, err := Build(d, "/cfg", "U01ADMIN"); err == nil {
		t.Error("a native profile was built as sandboxed; there is no process to confine")
	}
}

// TestNativeBackendDropsTheGuards. The checkboxes are never shown for a native
// backend, so the defaults they carry must be cleared when the backend is
// chosen — otherwise the form fails validation over a box nobody was offered.
func TestNativeBackendDropsTheGuards(t *testing.T) {
	d := NewDraft()
	d.Kind = KindOpenAI
	next := d.Next()
	if next.Sandboxed || next.Restricted {
		t.Errorf("native draft kept guards (sandboxed=%v restricted=%v)", next.Sandboxed, next.Restricted)
	}
}

// TestRestrictedPinsCloudStateToTheWorkspace is the point of the option: the
// agent's cloud identity becomes its own rather than a second writer on the
// operator's ~/.config/gcloud and ~/.aws.
func TestRestrictedPinsCloudStateToTheWorkspace(t *testing.T) {
	d := claudeCodeDraft()
	d.Restricted = true

	out, err := Build(d, "/cfg", "U01ADMIN")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if out.Default.ClaudeCode == nil {
		t.Fatal("the claude_code backend block is missing")
	}
	env := out.Default.ClaudeCode.Env
	for key, want := range map[string]string{
		"CLOUDSDK_CONFIG":                filepath.Join("/srv/work", ".gcloud"),
		"GOOGLE_APPLICATION_CREDENTIALS": filepath.Join("/srv/work", ".gcloud", "credentials.json"),
		"AWS_CONFIG_FILE":                filepath.Join("/srv/work", ".aws", "config"),
		"AWS_SHARED_CREDENTIALS_FILE":    filepath.Join("/srv/work", ".aws", "shared_creds"),
		"GRADLE_USER_HOME":               filepath.Join("/srv/work", ".gradle"),
	} {
		if env[key] != want {
			t.Errorf("%s = %q, want %q", key, env[key], want)
		}
	}
	if env["CLOUDSDK_CORE_DISABLE_USAGE_REPORTING"] != "true" {
		t.Error("gcloud usage reporting was left on for an agent the operator never opted in for")
	}
}

// TestUnrestrictedLeavesTheEnvironmentAlone. The option is opt-in, and an
// unticked box must not leave a half-applied environment behind.
func TestUnrestrictedLeavesTheEnvironmentAlone(t *testing.T) {
	out, err := Build(claudeCodeDraft(), "/cfg", "U01ADMIN")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(out.Default.ClaudeCode.Env) != 0 {
		t.Errorf("env = %v, want empty without the restricted option", out.Default.ClaudeCode.Env)
	}
}

// TestSandboxModesMatchTheHost keeps the picker honest: offering a mode this
// build cannot apply produces a profile that fails at the next restart, far
// from the checkbox that caused it.
func TestSandboxModesMatchTheHost(t *testing.T) {
	modes := AvailableSandboxModes()
	if runtime.GOOS == "darwin" {
		if len(modes) == 0 {
			t.Fatal("no sandbox mode offered on macOS, where seatbelt exists")
		}
		if modes[0].Mode != config.SandboxModeSeatbelt {
			t.Errorf("macOS offers %q, want %q", modes[0].Mode, config.SandboxModeSeatbelt)
		}
		return
	}
	if len(modes) != 0 {
		t.Errorf("%s offers %v, but has no confinement to apply", runtime.GOOS, modes)
	}
}

// TestDefaultToolFamiliesAreInTheCatalogue guards the pre-selection against a
// typo, which would otherwise read as a family the operator quietly did not get.
func TestDefaultToolFamiliesAreInTheCatalogue(t *testing.T) {
	for _, name := range DefaultToolFamilies() {
		if _, ok := ToolFamilyFor(name); !ok {
			t.Errorf("default family %q is not in the catalogue", name)
		}
	}
}
