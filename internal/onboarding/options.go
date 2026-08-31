package onboarding

import (
	"path/filepath"
	"runtime"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/toolset"
)

// This file holds the three answers the form's last step collects: which tool
// families the general agent may call, whether its process is confined, and
// whether its cloud SDKs are pinned inside its own workspace.
//
// They live together because they are the same question asked three ways —
// how much of this machine does the agent everybody talks to get to touch —
// and because a form that offered any one of them alone would imply the other
// two were already decided.

// ToolFamily is one selectable entry in the form's tool picker.
//
// A family is an allowlist entry as toolset.Resolve understands it: either a
// synthesized native group (files/terminal/skills/attach) or a registry
// namespace that selects every tool under it ("slack" pulls in slack.send-msg,
// slack.fetch-msgs, …). The catalogue is curated rather than derived from the
// registry because a picker needs what a registry cannot supply — a label, a
// sentence of consequence, and a considered default. internal/app's
// TestToolFamilyCatalogueCoversTheRegistry keeps it from drifting behind a
// newly registered namespace.
type ToolFamily struct {
	// Name is the value written into AgentProfile.Tools.
	Name string
	// Label is the option's title in the picker.
	Label string
	// Description is the line under it. Slack truncates past 75 characters, so
	// these say what the family lets the agent do, not how it works.
	Description string
	// Default marks the families pre-selected for the general profile.
	Default bool
	// AdminOnly marks a family that can reconfigure or restart Murtaugh itself.
	// It is offered — an operator may have a reason — but never pre-selected,
	// and the picker says so.
	AdminOnly bool
}

// ToolFamilies is the catalogue, in the order the picker presents it.
//
// The default set is deliberately small: the agent everybody talks to can hold
// a conversation (ask, present_plan), answer in Slack, work inside its own
// workspace (files, terminal), hand a file back (attach), and re-authenticate
// itself when a token lapses (auth). Everything past that is a decision
// somebody should make on purpose.
var ToolFamilies = []ToolFamily{
	{Name: "ask", Label: "Ask", Description: "Pause mid-turn to ask you a question.", Default: true},
	{Name: "present_plan", Label: "Present plan", Description: "Get your sign-off on a plan before acting."},
	{Name: "auth", Label: "Auth", Description: "Ask you to re-authenticate a lapsed credential.", Default: true},
	{Name: "attach", Label: "Attach", Description: "Return a workspace file to you as an upload.", Default: true},
	{Name: "slack", Label: "Slack", Description: "Post, read and react to messages.", Default: true},
	{Name: toolset.GroupFiles, Label: "Files", Description: "Read and write inside its own workspace.", Default: true},
	{Name: toolset.GroupTerminal, Label: "Terminal", Description: "Run commands in its workspace, behind the approval gate.", Default: true},
	{Name: toolset.GroupSkills, Label: "Skills", Description: "Read the bundled how-to skills."},
	{Name: "jobs", Label: "Jobs", Description: "Run and define scheduled jobs."},
	{Name: "journal", Label: "Journal", Description: "Query and trim the event journal."},
	{Name: "troubleshoot", Label: "Troubleshoot", Description: "Collect a diagnostics bundle."},
	{Name: "ping", Label: "Ping", Description: "Check that the tool surface answers."},
	{Name: "version", Label: "Version", Description: "Report the running version."},
	{Name: toolset.GroupManage, Label: "Manage", Description: "See the skills that teach configuring Murtaugh.", AdminOnly: true},
	{Name: "cfg", Label: "Config", Description: "Read and rewrite Murtaugh's own configuration.", AdminOnly: true},
	{Name: "setup", Label: "Setup", Description: "Install and reconfigure Murtaugh. Never bridged to a process agent.", AdminOnly: true},
	{Name: "restart", Label: "Restart", Description: "Restart the daemon.", AdminOnly: true},
}

// ToolFamilyFor returns the catalogue entry for name.
func ToolFamilyFor(name string) (ToolFamily, bool) {
	for _, f := range ToolFamilies {
		if f.Name == name {
			return f, true
		}
	}
	return ToolFamily{}, false
}

// DefaultToolFamilies is the general profile's pre-selected allowlist.
func DefaultToolFamilies() []string {
	out := make([]string, 0, len(ToolFamilies))
	for _, f := range ToolFamilies {
		if f.Default {
			out = append(out, f.Name)
		}
	}
	return out
}

// AllToolFamilies is every family in the catalogue.
//
// This is what the tweaker gets. It is the administrator's own agent, reachable
// from one person's DMs, and its entire job is changing Murtaugh — an install
// it cannot finish because a family was withheld is a worse failure than the
// breadth, and the operator is the one accountable for their own machine.
func AllToolFamilies() []string {
	out := make([]string, 0, len(ToolFamilies))
	for _, f := range ToolFamilies {
		out = append(out, f.Name)
	}
	return out
}

// SandboxOption is one confinement the form can offer.
//
// Confinement is OS-specific — seatbelt is a macOS facility with no portable
// equivalent — so the catalogue records which platforms each mode runs on and
// AvailableSandboxModes filters to the one Murtaugh is actually running on.
// Offering a mode this host cannot apply would produce a profile that fails to
// build at the next restart, reported nowhere near the checkbox that caused it.
type SandboxOption struct {
	Mode  string
	Label string
	// Description is the line under the checkbox.
	Description string
	// GOOS lists the runtime.GOOS values this mode supports.
	GOOS []string
}

// sandboxOptions is the confinement catalogue.
var sandboxOptions = []SandboxOption{{
	Mode:        config.SandboxModeSeatbelt,
	Label:       "Sandboxed",
	Description: "Confine the agent's process to its workspace with macOS seatbelt.",
	GOOS:        []string{"darwin"},
}}

// AvailableSandboxModes returns the confinements this host can apply.
func AvailableSandboxModes() []SandboxOption {
	var out []SandboxOption
	for _, opt := range sandboxOptions {
		for _, goos := range opt.GOOS {
			if goos == runtime.GOOS {
				out = append(out, opt)
				break
			}
		}
	}
	return out
}

// DefaultSandboxMode is the confinement to apply when the operator asks for
// one, and whether this host has any to give.
func DefaultSandboxMode() (string, bool) {
	modes := AvailableSandboxModes()
	if len(modes) == 0 {
		return "", false
	}
	return modes[0].Mode, true
}

// RestrictedEnv pins the cloud SDKs' state inside the agent's own workspace.
//
// Both tools default to a path under $HOME — ~/.config/gcloud, ~/.aws — which
// the operator's own shell also uses. An agent running `gcloud auth login` or
// `aws configure` there does not just read the operator's credentials, it
// rewrites the active configuration out from under them. Pointing each at the
// workspace it is already confined to makes the agent's cloud identity its own:
// separate from the operator's, and thrown away with the workspace.
//
// Deliberately credentials only. Redirecting a build tool's cache (Gradle,
// npm, Go module) would isolate no secret and cost a full re-download on the
// agent's first build, which is a tax without a benefit.
//
// The values are absolute paths rather than ${VAR} references because the
// workspace is known here and an unexpanded reference would resolve against the
// daemon's environment, not the agent's.
func RestrictedEnv(workDir string) map[string]string {
	return map[string]string{
		"CLOUDSDK_CONFIG": filepath.Join(workDir, ".gcloud"),
		// gcloud phones home about command usage by default. An agent runs far
		// more commands than a human, from a machine the operator did not opt in
		// on their behalf.
		"CLOUDSDK_CORE_DISABLE_USAGE_REPORTING": "true",
		// The filename is not ours to choose: `gcloud auth application-default
		// login` writes application_default_credentials.json into CLOUDSDK_CONFIG,
		// and GOOGLE_APPLICATION_CREDENTIALS has to name the file gcloud actually
		// produces. Any other name points at something that never appears, and
		// the SDKs fail to load ADC rather than falling back.
		"GOOGLE_APPLICATION_CREDENTIALS": filepath.Join(workDir, ".gcloud", "application_default_credentials.json"),
		"AWS_CONFIG_FILE":                       filepath.Join(workDir, ".aws", "config"),
		"AWS_SHARED_CREDENTIALS_FILE":           filepath.Join(workDir, ".aws", "shared_creds"),
	}
}
