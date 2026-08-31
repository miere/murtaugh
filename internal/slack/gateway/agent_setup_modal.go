package gateway

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh/internal/onboarding"
	"github.com/miere/murtaugh/internal/slack/agentcard"
)

// This file builds the agent-setup modal, one view per step.
//
// The form advances by REPLACING itself rather than by an in-body "Continue"
// button, because Slack requires a modal containing input blocks to have a
// submit button — so the submit IS the continue, and each submission either
// renders the next step or, on the last one, applies the answers. The step is
// carried in private_metadata; see internal/onboarding.Draft.

// Block and action identifiers for the form's inputs. Each pair is read back
// out of view.state.values, so they are constants rather than generated: a
// mismatch here reads as a silently empty field.
const (
	blockName    = "murtaugh_agent_setup_name"
	actionName   = "name"
	blockKind    = "murtaugh_agent_setup_kind"
	actionKind   = "kind"
	blockCommand = "murtaugh_agent_setup_command"
	actionComm   = "command"
	blockBaseURL = "murtaugh_agent_setup_base_url"
	actionBase   = "base_url"
	blockKeyEnv  = "murtaugh_agent_setup_key_env"
	actionKeyEnv = "key_env"
	blockKey     = "murtaugh_agent_setup_key"
	actionKey    = "key"
	blockModel   = "murtaugh_agent_setup_model"
	actionModel  = "model"
	blockWorkDir = "murtaugh_agent_setup_workdir"
	actionWork   = "workdir"
	blockTools   = "murtaugh_agent_setup_tools"
	actionTools  = "tools"
	blockGuards  = "murtaugh_agent_setup_guards"
	actionGuards = "guards"
)

// Values of the two guards. They share one checkbox group because they answer
// the same question — how much of this machine the agent reaches — and reading
// that as one list is simpler than two blocks that must both be present.
const (
	guardSandboxed  = "sandboxed"
	guardRestricted = "restricted"
)

func plain(text string) *slack.TextBlockObject {
	return slack.NewTextBlockObject(slack.PlainTextType, text, false, false)
}

func mrkdwn(text string) *slack.TextBlockObject {
	return slack.NewTextBlockObject(slack.MarkdownType, text, false, false)
}

// buildSetupModal renders the view for the draft's current step.
func buildSetupModal(d onboarding.Draft) (slack.ModalViewRequest, error) {
	metadata, err := d.Encode()
	if err != nil {
		return slack.ModalViewRequest{}, err
	}

	var (
		blocks []slack.Block
		submit string
	)
	switch d.Step {
	case onboarding.StepProvider:
		blocks, submit = providerStep(d)
	case onboarding.StepCredentials:
		blocks, submit = credentialsStep(d)
	case onboarding.StepModel:
		blocks, submit = modelStep(d)
	case onboarding.StepOptions:
		blocks, submit = optionsStep(d)
	default:
		return slack.ModalViewRequest{}, fmt.Errorf("unknown setup step %q", d.Step)
	}

	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      agentcard.ModalCallbackID,
		PrivateMetadata: metadata,
		Title:           plain("New Agent Profile"),
		Submit:          plain(submit),
		Close:           plain("Cancel"),
		Blocks:          slack.Blocks{BlockSet: blocks},
	}, nil
}

// providerStep asks for a name and a backend.
func providerStep(d onboarding.Draft) ([]slack.Block, string) {
	options := make([]*slack.OptionBlockObject, 0, len(onboarding.Providers))
	for _, p := range onboarding.Providers {
		options = append(options, slack.NewOptionBlockObject(string(p.Kind), plain(p.Label), nil))
	}
	radio := slack.NewRadioButtonsBlockElement(actionKind, options...)
	if d.Kind != "" {
		for _, opt := range options {
			if opt.Value == string(d.Kind) {
				radio.InitialOption = opt
			}
		}
	}

	name := slack.NewPlainTextInputBlockElement(plain("default"), actionName)
	name.InitialValue = d.Name

	return []slack.Block{
		slack.NewInputBlock(blockName, plain("Profile name"),
			plain("The agent everybody talks to. Your own DMs get a separate `tweaker` profile."), name),
		slack.NewInputBlock(blockKind, plain("Which agent type should this profile use?"), nil, radio),
	}, "Continue"
}

// credentialsStep asks for the credential, and the endpoint where one is
// meaningful.
//
// The base URL is offered only for the backends that have more than one
// endpoint. Gemini and Claude Code each have exactly one, so the field would be
// a box that does nothing — and a form that asks for things which do not apply
// is the clutter this multi-step design exists to avoid.
func credentialsStep(d onboarding.Draft) ([]slack.Block, string) {
	provider, _ := onboarding.ProviderFor(d.Kind)

	blocks := []slack.Block{
		slack.NewSectionBlock(mrkdwn(
			fmt.Sprintf("*%s* needs a credential before I can ask it which models you can use.", provider.Label)),
			nil, nil),
	}

	if provider.NeedsBaseURL {
		base := slack.NewPlainTextInputBlockElement(plain("https://api.example.com/v1"), actionBase)
		base.InitialValue = d.BaseURL
		blocks = append(blocks, optionalInput(blockBaseURL, "Endpoint URL",
			"Leave blank for the provider's own API. Set it for a compatible third party.", base))
	}

	keyEnv := slack.NewPlainTextInputBlockElement(plain(provider.DefaultKeyEnv), actionKeyEnv)
	keyEnv.InitialValue = firstNonEmpty(d.KeyEnv, provider.DefaultKeyEnv)
	blocks = append(blocks, slack.NewInputBlock(blockKeyEnv, plain("Environment variable"),
		plain("The credential is stored in .env under this name; the profile only references it."), keyEnv))

	key := slack.NewPlainTextInputBlockElement(plain("paste the key"), actionKey)
	key.InitialValue = d.Key
	blocks = append(blocks, slack.NewInputBlock(blockKey, plain("API key"),
		plain("Stored in plain text in .env beside your config."), key))

	return blocks, "Fetch models"
}

// modelStep asks for the model and the workspace.
func modelStep(d onboarding.Draft) ([]slack.Block, string) {
	var blocks []slack.Block

	if d.Kind == onboarding.KindClaudeCode {
		command := slack.NewPlainTextInputBlockElement(plain("claude"), actionComm)
		command.InitialValue = firstNonEmpty(d.Command, "claude")
		blocks = append(blocks, slack.NewInputBlock(blockCommand, plain("Claude Code command"),
			plain("Assumed to be already authenticated on this machine."), command))
	}

	options := make([]*slack.OptionBlockObject, 0, len(d.Models))
	for _, model := range d.Models {
		// Slack rejects an option label over 75 characters, and some
		// OpenAI-compatible endpoints return ids longer than that.
		options = append(options, slack.NewOptionBlockObject(model, plain(truncate(model, 75)), nil))
	}
	models := slack.NewOptionsSelectBlockElement(slack.OptTypeStatic, plain("Select a model"), actionModel, options...)
	blocks = append(blocks, slack.NewInputBlock(blockModel, plain("Model"), nil, models))

	work := slack.NewPlainTextInputBlockElement(plain("${HOME}/Murtaugh"), actionWork)
	work.InitialValue = firstNonEmpty(d.WorkDir, "${HOME}/Murtaugh")
	blocks = append(blocks, slack.NewInputBlock(blockWorkDir, plain("Work directory"),
		plain("Where the general agent may read and write. Your tweaker profile is rooted at the config directory instead."), work))

	return blocks, "Continue"
}

// optionsStep asks how far the general agent is trusted, and is the last step.
//
// The guards are shown only where they can be honoured: confinement needs a
// process to confine and a host that can confine it, and pinning the cloud SDKs
// needs a process environment to pin them in. A native backend has neither, so
// for it this page is the tool picker alone rather than two boxes that would
// silently do nothing.
func optionsStep(d onboarding.Draft) ([]slack.Block, string) {
	blocks := []slack.Block{
		slack.NewSectionBlock(mrkdwn(
			"Last one. This decides what the *general* agent may reach — your own `tweaker` profile always gets everything."),
			nil, nil),
	}

	options := make([]*slack.OptionBlockObject, 0, len(onboarding.ToolFamilies))
	initial := make([]*slack.OptionBlockObject, 0, len(onboarding.ToolFamilies))
	chosen := make(map[string]bool, len(d.Tools))
	for _, name := range d.EffectiveTools() {
		chosen[name] = true
	}
	for _, family := range onboarding.ToolFamilies {
		label := family.Label
		if family.AdminOnly {
			label += " (admin)"
		}
		opt := slack.NewOptionBlockObject(family.Name,
			plain(truncate(label, 75)), plain(truncate(family.Description, 75)))
		options = append(options, opt)
		if chosen[family.Name] {
			initial = append(initial, opt)
		}
	}
	tools := slack.NewOptionsMultiSelectBlockElement(slack.MultiOptTypeStatic,
		plain("Select tool families"), actionTools, options...)
	if len(initial) > 0 {
		tools.WithInitialOptions(initial...)
	}
	// Optional so the operator can clear it: an agent with no tools is a
	// legitimate choice (it can still hold a conversation), and Slack would
	// otherwise refuse to submit an emptied picker.
	blocks = append(blocks, optionalInput(blockTools, "Tool families",
		"What the general agent may call. The ones marked (admin) can reconfigure Murtaugh itself.", tools))

	if guards := guardOptions(d); len(guards) > 0 {
		group := slack.NewCheckboxGroupsBlockElement(actionGuards, guards...)
		var checked []*slack.OptionBlockObject
		for _, opt := range guards {
			if (opt.Value == guardSandboxed && d.Sandboxed) || (opt.Value == guardRestricted && d.Restricted) {
				checked = append(checked, opt)
			}
		}
		group.InitialOptions = checked
		blocks = append(blocks, optionalInput(blockGuards, "Confinement",
			"Both apply to the general agent's process only.", group))
	}

	return blocks, "Apply"
}

// guardOptions returns the confinement checkboxes this draft can honour, in
// display order. Empty for a backend or a host that supports neither.
func guardOptions(d onboarding.Draft) []*slack.OptionBlockObject {
	if !d.ProcessBacked() {
		return nil
	}
	var out []*slack.OptionBlockObject
	for _, mode := range onboarding.AvailableSandboxModes() {
		out = append(out, slack.NewOptionBlockObject(guardSandboxed,
			plain(truncate(mode.Label, 75)), plain(truncate(mode.Description, 75))))
		// One checkbox, not one per mode: the profile takes a single sandbox
		// mode, so a second option could only ever contradict the first.
		break
	}
	out = append(out, slack.NewOptionBlockObject(guardRestricted,
		plain("Agent restricted"),
		plain("Point its gcloud and AWS credentials at its workspace, not your home.")))
	return out
}

// waitingModal is shown while a provider is asked for its models.
//
// It exists because Slack expects a submission acknowledged within three
// seconds and a provider probe can take longer. Acknowledging with this, then
// replacing it when the answer lands, keeps the form responsive without
// pretending the network is instant. It carries no input blocks, so it needs no
// submit button.
func waitingModal(d onboarding.Draft) (slack.ModalViewRequest, error) {
	provider, _ := onboarding.ProviderFor(d.Kind)
	metadata, err := d.Encode()
	if err != nil {
		return slack.ModalViewRequest{}, err
	}
	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      agentcard.ModalCallbackID,
		PrivateMetadata: metadata,
		Title:           plain("New Agent Profile"),
		Close:           plain("Cancel"),
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			slack.NewSectionBlock(mrkdwn(
				fmt.Sprintf(":hourglass_flowing_sand: Asking *%s* which models your key can use…", provider.Label)),
				nil, nil),
		}},
	}, nil
}

// errorModal reports a failure the operator can act on, keeping the answers
// they already gave so a wrong key costs one field rather than the whole form.
func errorModal(d onboarding.Draft, message string) (slack.ModalViewRequest, error) {
	back := d
	back.Step = onboarding.StepCredentials
	metadata, err := back.Encode()
	if err != nil {
		return slack.ModalViewRequest{}, err
	}
	blocks, submit := credentialsStep(back)
	blocks = append([]slack.Block{
		slack.NewSectionBlock(mrkdwn(":warning: "+message), nil, nil),
		slack.NewDividerBlock(),
	}, blocks...)

	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      agentcard.ModalCallbackID,
		PrivateMetadata: metadata,
		Title:           plain("New Agent Profile"),
		Submit:          plain(submit),
		Close:           plain("Cancel"),
		Blocks:          slack.Blocks{BlockSet: blocks},
	}, nil
}

// optionalInput builds an input block Slack will accept empty.
func optionalInput(blockID, label, hint string, element slack.BlockElement) *slack.InputBlock {
	input := slack.NewInputBlock(blockID, plain(label), plain(hint), element)
	input.Optional = true
	return input
}

// readDraft folds a submitted view's values into the draft it was rendered
// from. Absent fields are left alone, so a step that does not show a field
// cannot blank what an earlier step collected.
func readDraft(d onboarding.Draft, state *slack.ViewState) onboarding.Draft {
	if state == nil {
		return d
	}
	value := func(blockID, actionID string) string {
		block, ok := state.Values[blockID]
		if !ok {
			return ""
		}
		return strings.TrimSpace(block[actionID].Value)
	}
	selected := func(blockID, actionID string) string {
		block, ok := state.Values[blockID]
		if !ok {
			return ""
		}
		action := block[actionID]
		if action.SelectedOption.Value != "" {
			return action.SelectedOption.Value
		}
		return ""
	}
	multiSelected := func(blockID, actionID string) []string {
		block, ok := state.Values[blockID]
		if !ok {
			return nil
		}
		out := make([]string, 0, len(block[actionID].SelectedOptions))
		for _, opt := range block[actionID].SelectedOptions {
			if v := strings.TrimSpace(opt.Value); v != "" {
				out = append(out, v)
			}
		}
		return out
	}

	if v := value(blockName, actionName); v != "" {
		d.Name = v
	}
	if v := selected(blockKind, actionKind); v != "" {
		d.Kind = onboarding.Kind(v)
	}
	if v := value(blockCommand, actionComm); v != "" {
		d.Command = v
	}
	// The endpoint is the one field a blank submission must be able to clear:
	// it is optional, and an operator correcting a wrong URL to "use the
	// provider default" has no other way to say so.
	if _, shown := state.Values[blockBaseURL]; shown {
		d.BaseURL = value(blockBaseURL, actionBase)
	}
	if v := value(blockKeyEnv, actionKeyEnv); v != "" {
		d.KeyEnv = v
	}
	if v := value(blockKey, actionKey); v != "" {
		d.Key = v
	}
	if v := selected(blockModel, actionModel); v != "" {
		d.Model = v
	}
	if v := value(blockWorkDir, actionWork); v != "" {
		d.WorkDir = v
	}
	// Both of these are read by PRESENCE of the block, not by a non-empty
	// value, for the same reason the endpoint is: unticking every box is a real
	// answer, and the "leave it alone when blank" rule the fields above follow
	// would make it impossible to give.
	if _, shown := state.Values[blockTools]; shown {
		d.Tools = multiSelected(blockTools, actionTools)
		d.ToolsChosen = true
	}
	if _, shown := state.Values[blockGuards]; shown {
		guards := multiSelected(blockGuards, actionGuards)
		d.Sandboxed = slices.Contains(guards, guardSandboxed)
		d.Restricted = slices.Contains(guards, guardRestricted)
	}
	return d
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// truncate shortens a label to max CHARACTERS.
//
// Rune-counted, not byte-counted: Slack's limits are on characters, and the
// ellipsis alone is three bytes — so a byte-based trim silently overshoots on
// exactly the long labels it was meant to handle, and Slack rejects the whole
// view rather than the one option.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max-1]) + "…"
}
