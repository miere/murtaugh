package gateway

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/slack-go/slack"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/onboarding"
	"github.com/miere/murtaugh/internal/slack/agentcard"
)

// blockIDs lists the input blocks a rendered view carries, which is the only
// thing that decides what the operator is asked for.
func blockIDs(t *testing.T, view slack.ModalViewRequest) []string {
	t.Helper()
	var ids []string
	for _, block := range view.Blocks.BlockSet {
		if input, ok := block.(*slack.InputBlock); ok {
			ids = append(ids, input.BlockID)
		}
	}
	return ids
}

func hasBlock(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestProviderStepAsksOnlyNameAndType keeps the first page short. The whole
// reason the form is a state machine is that a single page carrying every
// field for every backend asks most people for most things that do not apply.
func TestProviderStepAsksOnlyNameAndType(t *testing.T) {
	view, err := buildSetupModal(onboarding.NewDraft())
	if err != nil {
		t.Fatalf("buildSetupModal: %v", err)
	}
	ids := blockIDs(t, view)
	if len(ids) != 2 || !hasBlock(ids, blockName) || !hasBlock(ids, blockKind) {
		t.Errorf("first step asks for %v, want just the name and the type", ids)
	}
	if view.CallbackID != agentcard.ModalCallbackID {
		t.Errorf("callback id = %q, want %q", view.CallbackID, agentcard.ModalCallbackID)
	}
	// Slack requires a submit on any view containing inputs; without one the
	// form cannot advance at all.
	if view.Submit == nil || view.Submit.Text == "" {
		t.Error("a view with input blocks has no submit button")
	}
}

// TestCredentialStepShowsBaseURLOnlyWhereItApplies is the conditional-field
// rule: Gemini and Claude Code have exactly one endpoint each, so the box would
// do nothing.
func TestCredentialStepShowsBaseURLOnlyWhereItApplies(t *testing.T) {
	for kind, wantBaseURL := range map[onboarding.Kind]bool{
		onboarding.KindGemini:    false,
		onboarding.KindAnthropic: true,
		onboarding.KindOpenAI:    true,
	} {
		t.Run(string(kind), func(t *testing.T) {
			d := onboarding.NewDraft()
			d.Kind = kind
			view, err := buildSetupModal(d.Next())
			if err != nil {
				t.Fatalf("buildSetupModal: %v", err)
			}
			ids := blockIDs(t, view)
			if got := hasBlock(ids, blockBaseURL); got != wantBaseURL {
				t.Errorf("base URL shown = %v, want %v (blocks: %v)", got, wantBaseURL, ids)
			}
			if !hasBlock(ids, blockKey) || !hasBlock(ids, blockKeyEnv) {
				t.Errorf("credential step is missing its key fields: %v", ids)
			}
		})
	}
}

// TestClaudeCodeSkipsStraightToModel covers the branch that avoids showing an
// empty credentials page for a backend that authenticates itself.
func TestClaudeCodeSkipsStraightToModel(t *testing.T) {
	d := onboarding.NewDraft()
	d.Kind = onboarding.KindClaudeCode

	view, err := buildSetupModal(d.Next())
	if err != nil {
		t.Fatalf("buildSetupModal: %v", err)
	}
	ids := blockIDs(t, view)
	if hasBlock(ids, blockKey) {
		t.Error("Claude Code was asked for an API key it does not use")
	}
	if !hasBlock(ids, blockCommand) {
		t.Errorf("Claude Code was not asked for its command: %v", ids)
	}
	if !hasBlock(ids, blockModel) || !hasBlock(ids, blockWorkDir) {
		t.Errorf("model step is missing fields: %v", ids)
	}
	if view.Submit.Text != "Apply" {
		t.Errorf("final step submit = %q, want Apply", view.Submit.Text)
	}
}

// TestModelStepOffersDiscoveredModels checks the fetched list reaches the form.
func TestModelStepOffersDiscoveredModels(t *testing.T) {
	d := onboarding.NewDraft()
	d.Kind = onboarding.KindOpenAI
	d.Step = onboarding.StepModel
	d.Models = []string{"gpt-5", "gpt-4o"}

	view, err := buildSetupModal(d)
	if err != nil {
		t.Fatalf("buildSetupModal: %v", err)
	}
	raw, err := json.Marshal(view.Blocks)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range d.Models {
		if !strings.Contains(string(raw), model) {
			t.Errorf("model %q is not offered:\n%s", model, raw)
		}
	}
}

// TestModelOptionLabelsFitSlacksLimit guards a real failure mode: Slack rejects
// an option label over 75 characters, and some OpenAI-compatible endpoints
// return model ids longer than that. One would fail the whole view.
func TestModelOptionLabelsFitSlacksLimit(t *testing.T) {
	d := onboarding.NewDraft()
	d.Kind = onboarding.KindOpenAI
	d.Step = onboarding.StepModel
	d.Models = []string{strings.Repeat("very-long-model-id-", 8)}

	view, err := buildSetupModal(d)
	if err != nil {
		t.Fatalf("buildSetupModal: %v", err)
	}
	for _, block := range view.Blocks.BlockSet {
		input, ok := block.(*slack.InputBlock)
		if !ok || input.BlockID != blockModel {
			continue
		}
		sel, ok := input.Element.(*slack.SelectBlockElement)
		if !ok {
			t.Fatalf("model element is %T, want a select", input.Element)
		}
		for _, opt := range sel.Options {
			// Slack counts characters, not bytes.
			if n := utf8.RuneCountInString(opt.Text.Text); n > 75 {
				t.Errorf("option label is %d characters; Slack rejects the view over 75", n)
			}
			// The VALUE must stay intact — it is what gets configured.
			if opt.Value != d.Models[0] {
				t.Errorf("option value was truncated: %q", opt.Value)
			}
		}
	}
}

// TestDraftSurvivesEveryStep is the state machine's core property: each view
// carries forward what earlier steps collected, so the last submission has
// everything.
func TestDraftSurvivesEveryStep(t *testing.T) {
	d := onboarding.NewDraft()
	d.Name = "helper"
	d.Kind = onboarding.KindOpenAI

	creds, err := buildSetupModal(d.Next())
	if err != nil {
		t.Fatalf("buildSetupModal: %v", err)
	}
	back, err := onboarding.DecodeDraft(creds.PrivateMetadata)
	if err != nil {
		t.Fatalf("DecodeDraft: %v", err)
	}
	if back.Name != "helper" || back.Kind != onboarding.KindOpenAI {
		t.Errorf("the credentials step lost earlier answers: %+v", back)
	}
}

// TestReadDraftFoldsInSubmittedValues covers the read-back from view state,
// including the one field a blank submission must be able to clear.
func TestReadDraftFoldsInSubmittedValues(t *testing.T) {
	base := onboarding.NewDraft()
	base.Kind = onboarding.KindOpenAI
	base.BaseURL = "https://old.example.com"
	base.Key = "sk-old"

	state := &slack.ViewState{Values: map[string]map[string]slack.BlockAction{
		blockName:    {actionName: {Value: "helper"}},
		blockKeyEnv:  {actionKeyEnv: {Value: "MY_KEY"}},
		blockKey:     {actionKey: {Value: "sk-new"}},
		blockBaseURL: {actionBase: {Value: ""}},
	}}

	got := readDraft(base, state)
	if got.Name != "helper" || got.KeyEnv != "MY_KEY" || got.Key != "sk-new" {
		t.Errorf("submitted values were not folded in: %+v", got)
	}
	// An operator correcting a wrong URL back to "use the provider default" has
	// no other way to say so, so a shown-but-blank endpoint must clear it.
	if got.BaseURL != "" {
		t.Errorf("base URL = %q; a blank submission of a shown field must clear it", got.BaseURL)
	}
	// A field the step did not show must not be blanked.
	if got.Kind != onboarding.KindOpenAI {
		t.Errorf("kind = %q; a field absent from this step was cleared", got.Kind)
	}
}

// TestErrorModalKeepsTheAnswers checks a rejected credential costs one field
// rather than the whole form.
func TestErrorModalKeepsTheAnswers(t *testing.T) {
	d := onboarding.NewDraft()
	d.Kind = onboarding.KindOpenAI
	d.Name = "helper"
	d.KeyEnv = "MY_KEY"
	d.BaseURL = "https://api.example.com/v1"

	view, err := errorModal(d, "401 invalid api key")
	if err != nil {
		t.Fatalf("errorModal: %v", err)
	}
	raw, _ := json.Marshal(view.Blocks)
	if !strings.Contains(string(raw), "invalid api key") {
		t.Errorf("the provider's explanation was dropped:\n%s", raw)
	}
	back, err := onboarding.DecodeDraft(view.PrivateMetadata)
	if err != nil {
		t.Fatalf("DecodeDraft: %v", err)
	}
	if back.Name != "helper" || back.KeyEnv != "MY_KEY" {
		t.Errorf("the error view lost earlier answers: %+v", back)
	}
	if back.Step != onboarding.StepCredentials {
		t.Errorf("step = %q after an error, want the credentials step", back.Step)
	}
}

// TestSetupInteractionRouting pins the predicates. A missed match sends a click
// to the workflow engine; a false one steals somebody else's button.
func TestSetupInteractionRouting(t *testing.T) {
	if !isAgentSetupOpen(clickOn(agentcard.ActionOpen, "U01ADMIN")) {
		t.Error("the setup button was not recognised")
	}
	if isAgentSetupOpen(clickOn("murtaugh_restart_suggestion_confirm", "U01ADMIN")) {
		t.Error("the restart button was claimed as agent setup")
	}

	submit := slack.InteractionCallback{Type: slack.InteractionTypeViewSubmission}
	submit.View.CallbackID = agentcard.ModalCallbackID
	if !isAgentSetupSubmit(submit) {
		t.Error("the setup modal's submission was not recognised")
	}

	other := slack.InteractionCallback{Type: slack.InteractionTypeViewSubmission}
	other.View.CallbackID = "app_home_restart_confirm"
	if isAgentSetupSubmit(other) {
		t.Error("another modal's submission was claimed as agent setup")
	}
}

// TestPromptCardOffersTheForm checks the nudge carries the button that starts
// everything, and reads as a warning rather than a nicety.
func TestPromptCardOffersTheForm(t *testing.T) {
	blocks, err := agentcard.NewRenderer(t.TempDir(), assets.FS).Prompt()
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	body := string(blocks)
	if !strings.Contains(body, agentcard.ActionOpen) {
		t.Errorf("the prompt carries no setup button:\n%s", body)
	}
	if !strings.Contains(body, "no agent configured") {
		t.Errorf("the prompt does not say what is wrong:\n%s", body)
	}
	var payload map[string]any
	if err := json.Unmarshal(blocks, &payload); err != nil {
		t.Fatalf("the prompt is not valid JSON: %v\n%s", err, body)
	}
}

// TestSettledPromptDropsTheButton stops a stale card in the DM being clicked
// into a second setup after one has already completed.
func TestSettledPromptDropsTheButton(t *testing.T) {
	blocks, err := agentcard.NewRenderer(t.TempDir(), assets.FS).Settled("Configured by <@U01ADMIN>.")
	if err != nil {
		t.Fatalf("Settled: %v", err)
	}
	if strings.Contains(string(blocks), agentcard.ActionOpen) {
		t.Errorf("a settled prompt still carries its button:\n%s", blocks)
	}
}
