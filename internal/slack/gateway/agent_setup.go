package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/miere/murtaugh/internal/onboarding"
	"github.com/miere/murtaugh/internal/slack/agentcard"
	"github.com/miere/murtaugh/internal/slack/alertcard"
)

// This file drives the agent-setup conversation: notice that no agent exists,
// offer the form, walk its steps, and apply the answers.
//
// It is the last leg of an install that now begins in a terminal and ends in
// Slack. A daemon with no agent is running correctly and answering nobody,
// which from the operator's side looks exactly like a broken one — so it says
// so, unprompted, and hands them the form rather than a documentation link.

// AgentProfileWriter applies a completed form. The composition root supplies a
// closure over the config store and the .env writer, so this package stays free
// of both.
type AgentProfileWriter func(ctx context.Context, profiles onboarding.Profiles) error

// WithAgentProfileWriter enables the setup form. Without one the prompt is
// still shown but the form cannot be applied, so it is not offered at all.
func (a *Gateway) WithAgentProfileWriter(write AgentProfileWriter) *Gateway {
	a.writeAgentProfiles = write
	return a
}

// setupApplyTimeout bounds the store and .env writes a completed form makes.
const setupApplyTimeout = 30 * time.Second

// NotifyNoAgents tells the admin that nothing can answer yet, and offers the
// form.
//
// Best-effort and silent when there is nobody to tell: on a fresh install the
// admin is claimed by the first DM (see admin_claim.go), and this fires again
// on the next promotion once somebody has.
func (a *Gateway) NotifyNoAgents(ctx context.Context) {
	if a.writeAgentProfiles == nil || a.agentCards == nil {
		return
	}
	admin := strings.TrimSpace(a.access().AdminUser)
	if admin == "" {
		return
	}
	dest, err := a.resolveSuggestionDestination(ctx, "")
	if err != nil || dest == "" {
		a.logger.Warn("cannot prompt for agent setup: no admin conversation", "error", err)
		return
	}
	blocks, err := a.agentCards.Prompt()
	if err != nil {
		a.logger.Warn("could not render the agent setup prompt", "error", err)
		return
	}
	if _, err := a.postRawCard(ctx, dest, blocks, agentcard.PlainText()); err != nil {
		a.logger.Warn("could not post the agent setup prompt", "error", err)
	}
}

// isAgentSetupOpen reports a click on the prompt's button.
func isAgentSetupOpen(interaction slack.InteractionCallback) bool {
	for _, action := range interaction.ActionCallback.BlockActions {
		if action.ActionID == agentcard.ActionOpen {
			return true
		}
	}
	return false
}

// isAgentSetupSubmit reports a submission of any step of the form.
func isAgentSetupSubmit(interaction slack.InteractionCallback) bool {
	return interaction.Type == slack.InteractionTypeViewSubmission &&
		interaction.View.CallbackID == agentcard.ModalCallbackID
}

// handleAgentSetupOpen opens the form.
//
// It runs inline rather than on a goroutine because Slack expires a trigger_id
// within seconds, and a modal opened with an expired one fails silently.
func (a *Gateway) handleAgentSetupOpen(ctx context.Context, interaction slack.InteractionCallback) {
	if !a.access().IsAdminUser(interaction.User.ID) {
		a.logger.Warn("ignoring an agent setup click from a non-admin", "user", interaction.User.ID)
		return
	}
	if a.webClient == nil {
		return
	}
	view, err := buildSetupModal(onboarding.NewDraft())
	if err != nil {
		a.logger.Error("could not build the agent setup modal", "error", err)
		return
	}
	if _, err := a.webClient.OpenViewContext(ctx, interaction.TriggerID, view); err != nil {
		a.logger.Error("could not open the agent setup modal", "error", err)
	}
}

// handleAgentSetupSubmit advances or applies the form.
//
// It owns its acknowledgement, unlike every other interaction here. A modal
// that replaces itself can only do so in the acknowledgement of the submission
// that triggered it, so acking blank first — which is what the common path does
// — would close the form instead of advancing it.
func (a *Gateway) handleAgentSetupSubmit(event socketmode.Event, interaction slack.InteractionCallback) {
	if !a.access().IsAdminUser(interaction.User.ID) {
		a.logger.Warn("ignoring an agent setup submission from a non-admin", "user", interaction.User.ID)
		a.ack(event)
		return
	}

	draft, err := onboarding.DecodeDraft(interaction.View.PrivateMetadata)
	if err != nil {
		a.logger.Error("could not decode the agent setup draft", "error", err)
		a.ackViewErrors(event, map[string]string{blockName: "The form lost its place; please start again."})
		return
	}
	draft = readDraft(draft, interaction.View.State)

	switch draft.Step {
	case onboarding.StepProvider:
		a.advanceSetup(event, draft.Next())

	case onboarding.StepCredentials:
		// Discovery is a network call to somebody else's API and may outlast
		// the three seconds Slack allows for an acknowledgement, so the form is
		// parked on a waiting view and replaced when the answer lands.
		waiting, err := waitingModal(draft)
		if err != nil {
			a.logger.Error("could not build the waiting modal", "error", err)
			a.ack(event)
			return
		}
		a.ack(event, slack.NewUpdateViewSubmissionResponse(&waiting))
		go a.discoverAndAdvance(interaction.View.ID, draft)

	case onboarding.StepModel:
		a.advanceSetup(event, draft.Next())

	case onboarding.StepOptions:
		a.applySetup(event, interaction, draft)

	default:
		a.ack(event)
	}
}

// advanceSetup acknowledges a submission by replacing the modal with the next
// step.
func (a *Gateway) advanceSetup(event socketmode.Event, draft onboarding.Draft) {
	view, err := buildSetupModal(draft)
	if err != nil {
		a.logger.Error("could not build the next setup step", "error", err)
		a.ack(event)
		return
	}
	a.ack(event, slack.NewUpdateViewSubmissionResponse(&view))
}

// discoverAndAdvance asks the provider for its models and replaces the waiting
// view with the result.
func (a *Gateway) discoverAndAdvance(viewID string, draft onboarding.Draft) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	models, err := onboarding.DiscoverModels(ctx, a.modelProbe(), draft)
	if err != nil {
		// The provider's own words, not ours: "invalid api key" and "no such
		// host" send the operator to different fields, and only the provider
		// knows which it was.
		a.updateSetupView(ctx, viewID, func() (slack.ModalViewRequest, error) {
			return errorModal(draft, err.Error())
		})
		return
	}

	next := draft.Next()
	next.Models = models
	a.updateSetupView(ctx, viewID, func() (slack.ModalViewRequest, error) {
		return buildSetupModal(next)
	})
}

// updateSetupView replaces an open modal.
func (a *Gateway) updateSetupView(ctx context.Context, viewID string, build func() (slack.ModalViewRequest, error)) {
	if a.webClient == nil || viewID == "" {
		return
	}
	view, err := build()
	if err != nil {
		a.logger.Error("could not build the setup modal", "error", err)
		return
	}
	if _, err := a.webClient.UpdateViewContext(ctx, view, "", "", viewID); err != nil {
		a.logger.Error("could not update the setup modal", "error", err)
	}
}

// applySetup writes the profiles a completed form describes.
//
// Validation failures are returned to the modal as field errors rather than as
// a DM: the operator is looking at the form, and a message elsewhere about a
// box in front of them is a worse answer than marking the box.
func (a *Gateway) applySetup(event socketmode.Event, interaction slack.InteractionCallback, draft onboarding.Draft) {
	admin := strings.TrimSpace(a.access().AdminUser)
	profiles, err := onboarding.Build(draft, a.configDir, admin)
	if err != nil {
		// Marked against the tool picker, not the work directory: Slack drops a
		// field error naming a block the OPEN view does not contain, and by this
		// point the operator is looking at the options step.
		a.ackViewErrors(event, map[string]string{blockTools: err.Error()})
		return
	}
	if a.writeAgentProfiles == nil {
		// Marked against the tool picker, not the work directory: Slack drops a
		// field error naming a block the OPEN view does not contain, and by this
		// point the operator is looking at the options step.
		a.ackViewErrors(event, map[string]string{blockTools: "This build cannot write agent profiles."})
		return
	}

	// Acknowledge with a clear so the modal closes now: the write reconfigures
	// and reloads the daemon, which takes longer than Slack will wait, and the
	// outcome is reported in the DM where the prompt was.
	a.ack(event)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), setupApplyTimeout)
		defer cancel()

		if err := a.writeAgentProfiles(ctx, profiles); err != nil {
			a.logger.Error("could not apply the agent setup", "error", err)
			a.reportSetupOutcome(ctx, alertcard.Spec{
				Level:     alertcard.LevelError,
				Title:     "Could not save the agent profiles",
				Subtitle:  "Nothing was changed; the form can be reopened from the prompt above.",
				Detail:    err.Error(),
				NextSteps: "Check the daemon log, then try the form again.",
			})
			return
		}
		a.logger.Info("agent profiles created", "default", profiles.Name, "tweaker", onboarding.TweakerName)
		a.reportSetupOutcome(ctx, alertcard.Spec{
			Level:    alertcard.LevelNotice,
			Title:    fmt.Sprintf("Created %s and %s", profiles.Name, onboarding.TweakerName),
			Subtitle: "say hello when the reload finishes",
		})
	}()
}

// reportSetupOutcome DMs the operator the result of applying the form.
//
// Success is a notice rather than a card: it lands directly under the setup
// prompt that produced it, and a second full-width block there repeats what the
// reload notices already say. A failure keeps the card, because that one needs
// a body.
func (a *Gateway) reportSetupOutcome(ctx context.Context, spec alertcard.Spec) {
	dest, err := a.resolveSuggestionDestination(ctx, "")
	if err != nil || dest == "" {
		a.logger.Warn("could not report the agent setup outcome", "error", err)
		return
	}
	if _, _, err := a.postLifecycleAlert(ctx, dest, "", spec); err != nil {
		a.logger.Warn("could not report the agent setup outcome", "error", err)
	}
}

// ackViewErrors acknowledges a submission by marking fields in the open modal.
func (a *Gateway) ackViewErrors(event socketmode.Event, errs map[string]string) {
	a.ack(event, slack.NewErrorsViewSubmissionResponse(errs))
}

// modelProbe is the HTTP client used for model discovery.
//
// Deliberately NOT the leadership-gated client: these are calls to Anthropic,
// Google or an OpenAI-compatible endpoint, not to Slack, and routing them
// through a gate that only understands Slack methods would block them all.
func (a *Gateway) modelProbe() onboarding.Doer {
	if a.probeClient != nil {
		return a.probeClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// WithModelProbe overrides the HTTP client used to discover models. Tests use
// it to answer without a network.
func (a *Gateway) WithModelProbe(client onboarding.Doer) *Gateway {
	a.probeClient = client
	return a
}

// WithConfigDir tells the gateway where config.yaml lives, which is where the
// tweaker profile is rooted.
func (a *Gateway) WithConfigDir(dir string) *Gateway {
	a.configDir = dir
	return a
}
