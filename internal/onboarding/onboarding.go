// Package onboarding turns the answers to a short form into the two agent
// profiles a fresh Murtaugh needs, and discovers the models each backend can
// actually serve.
//
// It is deliberately free of Slack. The form happens to be a Slack modal today,
// and everything here — the provider catalogue, the draft's state machine, the
// model probes, the profiles that come out the other end — is the same whether
// the answers arrive from a modal, a CLI prompt, or a test.
package onboarding

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/config"
)

// Kind is an agent backend the form can configure.
type Kind string

const (
	// KindClaudeCode drives the `claude` binary. It authenticates itself, so
	// the form asks for no credential.
	KindClaudeCode Kind = "claude_code"
	// KindGemini is the native loop against Google's API.
	KindGemini Kind = "gemini"
	// KindAnthropic is the native loop against an Anthropic-compatible API.
	KindAnthropic Kind = "anthropic"
	// KindOpenAI is the native loop against an OpenAI-compatible API, which is
	// how GLM, DeepSeek, Kimi and self-hosted servers are reached.
	KindOpenAI Kind = "openai"
)

// Provider describes one choice on the form.
type Provider struct {
	Kind  Kind
	Label string
	// NeedsKey is whether the form must collect a credential.
	NeedsKey bool
	// NeedsBaseURL is whether an endpoint override is meaningful. Gemini and
	// Claude Code have exactly one endpoint each, so offering the field would
	// be a box that does nothing — which is the clutter the form is trying to
	// avoid.
	NeedsBaseURL bool
	// DefaultKeyEnv is the .env variable the credential is stored under.
	DefaultKeyEnv string
}

// Providers is the catalogue, in the order the form presents it.
var Providers = []Provider{
	{Kind: KindClaudeCode, Label: "Claude Code"},
	{Kind: KindGemini, Label: "Gemini", NeedsKey: true, DefaultKeyEnv: "GEMINI_API_KEY"},
	{Kind: KindAnthropic, Label: "Anthropic-compatible", NeedsKey: true, NeedsBaseURL: true, DefaultKeyEnv: "ANTHROPIC_API_KEY"},
	{Kind: KindOpenAI, Label: "OpenAI-compatible", NeedsKey: true, NeedsBaseURL: true, DefaultKeyEnv: "OPENAI_API_KEY"},
}

// ProviderFor returns the catalogue entry for kind.
func ProviderFor(kind Kind) (Provider, bool) {
	for _, p := range Providers {
		if p.Kind == kind {
			return p, true
		}
	}
	return Provider{}, false
}

// ClaudeCodeModels is the model list offered for Claude Code.
//
// Hard-coded, unlike every other backend here, because Claude Code exposes no
// machine-readable list: there is no `models` subcommand, `--help` does not
// enumerate them, and an unrecognised `--model` errors without suggesting
// alternatives. These are the aliases the binary resolves to whatever is
// current, which is the closest thing to a stable answer available — a pinned
// list of full model ids would go stale the week after it was written.
var ClaudeCodeModels = []string{"fable", "opus", "sonnet", "haiku"}

// TweakerName is the profile reserved for the administrator's own DMs.
const TweakerName = "tweaker"

// Step is where the form has got to.
//
// The form is a state machine rather than one page because the questions
// genuinely depend on each other: which credential to ask for depends on the
// backend, and the model list cannot be fetched until the credential exists.
// One page carrying every field for every backend would ask most people for
// most things that do not apply to them.
type Step string

const (
	// StepProvider asks for a name and a backend.
	StepProvider Step = "provider"
	// StepCredentials asks for the credential and endpoint. Skipped entirely
	// for a backend that authenticates itself.
	StepCredentials Step = "credentials"
	// StepModel asks for the model and the workspace.
	StepModel Step = "model"
	// StepOptions asks how far the general agent is trusted — which tool
	// families it may call, whether its process is confined, and whether its
	// cloud SDKs are pinned to its workspace — then submits.
	//
	// It comes last because each of its answers is scoped by an earlier one:
	// confinement means nothing for a backend that runs in-process, and the
	// cloud-SDK pinning needs the workspace the model step collected.
	StepOptions Step = "options"
)

// Draft is the form's accumulated state, carried between steps.
type Draft struct {
	Step Step `json:"step"`
	// Name is the general-purpose profile's name. The administrator's profile
	// is always TweakerName.
	Name string `json:"name,omitempty"`
	Kind Kind   `json:"kind,omitempty"`
	// Command is the claude binary, for KindClaudeCode.
	Command string `json:"command,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	// KeyEnv is the .env variable the credential is written to; Key is the
	// credential itself, held only until the profiles are built.
	KeyEnv string `json:"key_env,omitempty"`
	Key    string `json:"key,omitempty"`
	// Models is what discovery found, offered as the model choices.
	Models  []string `json:"models,omitempty"`
	Model   string   `json:"model,omitempty"`
	WorkDir string   `json:"workdir,omitempty"`
	// Tools is the general profile's tool allowlist, as chosen on the options
	// step. Distinguished from "not answered yet" by ToolsChosen, because an
	// operator clearing every box is a real answer — an agent that can hold a
	// conversation and nothing else — and must not silently re-seed the
	// defaults.
	Tools       []string `json:"tools,omitempty"`
	ToolsChosen bool     `json:"tools_chosen,omitempty"`
	// Sandboxed confines the general agent's process. Only meaningful for a
	// process backend on a host with a confinement to apply.
	Sandboxed bool `json:"sandboxed,omitempty"`
	// Restricted points the agent's cloud SDKs at its own workspace instead of
	// the operator's home directory. See RestrictedEnv.
	Restricted bool `json:"restricted,omitempty"`
}

// NewDraft starts a form.
//
// The tool defaults and the sandbox are pre-answered rather than left blank: a
// form whose safe choice is the one already ticked is answered correctly by an
// operator who clicks straight through it, and the confinement default is what
// onboarding applied before this step existed.
func NewDraft() Draft {
	_, canSandbox := DefaultSandboxMode()
	return Draft{
		Step:      StepProvider,
		Name:      "default",
		Command:   "claude",
		Tools:     DefaultToolFamilies(),
		Sandboxed: canSandbox,
	}
}

// Next advances the draft to the step its current answers imply.
//
// Claude Code skips credentials entirely rather than showing an empty page:
// it authenticates itself, so there is nothing to ask.
func (d Draft) Next() Draft {
	switch d.Step {
	case StepProvider:
		// The backend is only known now, and it decides whether the guards can
		// exist at all. Clearing them here rather than at Build keeps the
		// invariant next to the answer that creates it: a native draft never
		// carries a guard the options step will not show, so it cannot fail
		// validation over a box the operator was never offered.
		if !d.ProcessBacked() {
			d.Sandboxed = false
			d.Restricted = false
		}
		if p, ok := ProviderFor(d.Kind); ok && !p.NeedsKey {
			d.Step = StepModel
			d.Models = ClaudeCodeModels
			return d
		}
		d.Step = StepCredentials
		return d
	case StepModel:
		d.Step = StepOptions
		return d
	case StepOptions:
		// The last step. Advancing past it is the caller's bug, and moving the
		// form backwards would be a worse answer than standing still.
		return d
	default:
		d.Step = StepModel
		return d
	}
}

// ProcessBacked reports whether the draft's backend runs as a child process,
// which is what makes confinement and a per-agent environment meaningful. A
// native agent has neither: it runs in-process, so there is nothing to confine
// and no process environment of its own to set.
func (d Draft) ProcessBacked() bool { return d.Kind == KindClaudeCode }

// Validate reports whether the draft is complete enough to build profiles.
func (d Draft) Validate() error {
	if strings.TrimSpace(d.Name) == "" {
		return errors.New("a profile name is required")
	}
	if d.Name == TweakerName {
		return fmt.Errorf("%q is reserved for the administrator's own profile; choose another name", TweakerName)
	}
	provider, ok := ProviderFor(d.Kind)
	if !ok {
		return fmt.Errorf("unknown agent type %q", d.Kind)
	}
	if strings.TrimSpace(d.Model) == "" {
		return errors.New("a model is required")
	}
	if strings.TrimSpace(d.WorkDir) == "" {
		return errors.New("a work directory is required")
	}
	if provider.NeedsKey {
		if strings.TrimSpace(d.Key) == "" {
			return errors.New("an API key is required for this agent type")
		}
		if strings.TrimSpace(d.KeyEnv) == "" {
			return errors.New("an environment variable name is required to store the API key")
		}
	}
	if d.Kind == KindClaudeCode && strings.TrimSpace(d.Command) == "" {
		return errors.New("the claude command is required")
	}
	// Refused rather than silently downgraded: an operator who asked for
	// confinement and got none would believe the agent was confined, which is
	// worse than either honest outcome.
	if d.Sandboxed {
		if !d.ProcessBacked() {
			return errors.New("only a process-backed agent can be sandboxed; this backend runs in-process")
		}
		if _, ok := DefaultSandboxMode(); !ok {
			return fmt.Errorf("sandboxing is not available on %s", runtime.GOOS)
		}
	}
	if d.Restricted && !d.ProcessBacked() {
		return errors.New("only a process-backed agent has an environment of its own to restrict")
	}
	return nil
}

// EffectiveTools is the general profile's allowlist: what the operator chose,
// or the defaults until they have been asked.
func (d Draft) EffectiveTools() []string {
	if d.ToolsChosen {
		return append([]string(nil), d.Tools...)
	}
	if len(d.Tools) > 0 {
		return append([]string(nil), d.Tools...)
	}
	return DefaultToolFamilies()
}

// Encode renders the draft for a modal's private_metadata.
func (d Draft) Encode() (string, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("encode onboarding draft: %w", err)
	}
	// Slack caps private_metadata at 3000 characters, and a long discovered
	// model list is the only field that can approach it. Trimming the list
	// beats failing the whole form.
	for len(raw) > 3000 && len(d.Models) > 1 {
		d.Models = d.Models[:len(d.Models)/2]
		if raw, err = json.Marshal(d); err != nil {
			return "", fmt.Errorf("encode onboarding draft: %w", err)
		}
	}
	return string(raw), nil
}

// DecodeDraft parses a modal's private_metadata.
func DecodeDraft(raw string) (Draft, error) {
	var d Draft
	if strings.TrimSpace(raw) == "" {
		return NewDraft(), nil
	}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return Draft{}, fmt.Errorf("decode onboarding draft: %w", err)
	}
	return d, nil
}

// Profiles is what a completed form produces.
type Profiles struct {
	// Name is the general-purpose profile's name.
	Name string
	// Default answers everyone, everywhere.
	Default config.AgentProfile
	// Tweaker answers the administrator's direct messages only.
	Tweaker config.AgentProfile
	// Chat is the routing that binds them.
	Chat config.ChatConfig
	// DefaultWorkDir and TweakerWorkDir report where each profile is rooted.
	//
	// They are surfaced here rather than left for a caller to read back off the
	// profile because AgentProfile.WorkDir is write-once at the build seam:
	// downstream reads are refused by the architecture guard, which exists so a
	// workdir is resolved and validated in exactly one place.
	DefaultWorkDir string
	TweakerWorkDir string
	// EnvKey / EnvValue is the credential to write to .env, empty when the
	// backend authenticates itself.
	EnvKey   string
	EnvValue string
}

// Build turns a completed draft into the two profiles and the routing.
//
// # Why two profiles and not one
//
// They differ only in how much they are trusted, and that difference cannot be
// collapsed. The administrator needs an agent that can change Murtaugh itself —
// rooted in the config directory, unsandboxed, unprompted — because the whole
// point is to finish the install from Slack. Everybody else needs the opposite.
// One profile would have to pick, and either choice is wrong for somebody.
//
// configDir is where config.yaml lives; adminUser is the Slack ID the tweaker
// is bound to.
func Build(d Draft, configDir, adminUser string) (Profiles, error) {
	if err := d.Validate(); err != nil {
		return Profiles{}, err
	}
	if strings.TrimSpace(adminUser) == "" {
		return Profiles{}, errors.New("an administrator is required to bind the tweaker profile to")
	}

	name := strings.TrimSpace(d.Name)
	workDir := expandHome(strings.TrimSpace(d.WorkDir))

	general := d.backend()
	// Everyone's agent: it asks before running a terminal command, answers an
	// ACP agent's own permission requests by asking too, and is confined to the
	// workspace the operator named.
	general.WorkDir = workDir
	general.ProgressDisplay = string(config.ProgressDisplayTasks)
	general.Approval = config.ApprovalConfig{Terminal: "prompt", Requests: "ask"}
	// The allowlist is what decides whether the agent has any tools at all.
	// Left empty, toolset.Resolve selects nothing and the profile comes up
	// mute — able to talk, unable to act, with no error anywhere to say why.
	general.Tools = d.EffectiveTools()
	if d.ProcessBacked() {
		if d.Sandboxed {
			if mode, ok := DefaultSandboxMode(); ok {
				general.Sandbox = config.SandboxConfig{Mode: mode}
			}
		} else {
			general.Sandbox = config.SandboxConfig{Mode: config.SandboxModeOff}
		}
		if d.Restricted {
			setBackendEnv(&general, RestrictedEnv(workDir))
		}
	}

	tweaker := d.backend()
	// The administrator's agent: rooted where the configuration lives, with the
	// gates off, because it exists to change Murtaugh and every prompt would be
	// the operator approving their own request. It is reachable only from the
	// admin's DMs (see the routing below), which is what makes that safe.
	tweaker.WorkDir = configDir
	tweaker.ProgressDisplay = string(config.ProgressDisplayTasks)
	tweaker.Approval = config.ApprovalConfig{Terminal: "off", Requests: "auto-allow"}
	tweaker.Sandbox = config.SandboxConfig{Mode: config.SandboxModeOff}
	// Every tool family, for the same reason the gates are off: this profile
	// exists to configure Murtaugh, it answers one person's DMs, and a family
	// withheld here is an install the administrator cannot finish.
	tweaker.Tools = AllToolFamilies()
	// The bundled skills are what teach an agent how Murtaugh is configured, so
	// the profile whose job is reconfiguring it gets them on disk.
	tweaker.ExportSkillsToFS = bundledSkills()

	chat := config.ChatConfig{
		Enabled: true,
		Defaults: config.ChatDefaults{
			Agent: name,
			// Bound to the administrator by user, never through dm_agent: that
			// routes EVERY direct message, which would hand an unsandboxed,
			// ungated agent rooted in the credential directory to anyone on the
			// allowlist.
			DMAgents: map[string]string{adminUser: TweakerName},
		},
	}

	out := Profiles{
		Name:           name,
		Default:        general,
		Tweaker:        tweaker,
		Chat:           chat,
		DefaultWorkDir: workDir,
		TweakerWorkDir: configDir,
	}
	if provider, ok := ProviderFor(d.Kind); ok && provider.NeedsKey {
		out.EnvKey = strings.TrimSpace(d.KeyEnv)
		out.EnvValue = d.Key
	}
	return out, nil
}

// backend builds the kind-specific half of a profile.
func (d Draft) backend() config.AgentProfile {
	if d.Kind == KindClaudeCode {
		return config.AgentProfile{ClaudeCode: &config.ClaudeCodeProfile{
			Command: strings.TrimSpace(d.Command),
			Model:   strings.TrimSpace(d.Model),
		}}
	}
	return config.AgentProfile{Native: &config.NativeProfile{
		Provider:  string(d.Kind),
		Model:     strings.TrimSpace(d.Model),
		BaseURL:   strings.TrimSpace(d.BaseURL),
		APIKeyEnv: strings.TrimSpace(d.KeyEnv),
	}}
}

// setBackendEnv layers env onto whichever process backend the profile carries,
// leaving a native profile untouched (it has no process, so no environment).
// Existing entries win: this adds hardening, and silently overwriting something
// the operator set would be a surprise in the direction of breaking their agent.
func setBackendEnv(profile *config.AgentProfile, env map[string]string) {
	var target *map[string]string
	switch {
	case profile.ClaudeCode != nil:
		target = &profile.ClaudeCode.Env
	case profile.ACP != nil:
		target = &profile.ACP.Env
	default:
		return
	}
	if *target == nil {
		*target = make(map[string]string, len(env))
	}
	for key, value := range env {
		if _, exists := (*target)[key]; !exists {
			(*target)[key] = value
		}
	}
}

// bundledSkills lists every skill shipped in the binary, sorted so two runs
// produce the same profile.
func bundledSkills() []string {
	names := append([]string(nil), assets.SkillNames()...)
	sort.Strings(names)
	return names
}

// expandHome resolves a leading ~ or ${HOME} so the form's default value means
// what it looks like.
func expandHome(path string) string {
	if path == "" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	switch {
	case path == "~" || strings.HasPrefix(path, "~/"):
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
	case strings.HasPrefix(path, "${HOME}"):
		return filepath.Join(home, strings.TrimPrefix(path, "${HOME}"))
	case strings.HasPrefix(path, "$HOME"):
		return filepath.Join(home, strings.TrimPrefix(path, "$HOME"))
	default:
		return path
	}
}
