package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

const (
	// appHomeTab is the value Slack sends in app_home_opened for the Home
	// surface (as opposed to "messages").
	appHomeTab = "home"

	// appHomeUpdateActionID identifies the "Update" button rendered beside the
	// version on the admin's Home tab.
	appHomeUpdateActionID = "app_home_update"
	// appHomeVersionBlockID is the block holding the version line; a stable id
	// keeps the published view diffable.
	appHomeVersionBlockID = "app_home_version"
	// appHomeUpdateCallbackID tags the confirmation modal so the interaction
	// router can recognize its view_submission.
	appHomeUpdateCallbackID = "app_home_update_confirm"

	// appHomeRestartActionID identifies the "Restart Murtaugh" button on the
	// admin's Home tab. Unlike the Update button it is always offered to the
	// admin (no precondition), letting them trigger a graceful restart on
	// demand — the replacement for the old config-file-watch restart suggestion.
	appHomeRestartActionID = "app_home_restart"
	// appHomeRestartBlockID tags the actions block holding the restart button;
	// a stable id keeps the published view diffable.
	appHomeRestartBlockID = "app_home_restart_actions"
	// appHomeRestartCallbackID tags the restart confirmation modal so the
	// interaction router can recognize its view_submission.
	appHomeRestartCallbackID = "app_home_restart_confirm"
)

// handleAppHomeOpened publishes the control panel when a user opens the app's
// Home tab. Everyone sees the header and version; only the configured admin
// additionally sees an "Update" button when a newer release is available.
// app_home_opened also fires for the "messages" tab, which we ignore.
func (a *Gateway) handleAppHomeOpened(ev *slackevents.AppHomeOpenedEvent) {
	if ev == nil || ev.Tab != appHomeTab {
		return
	}
	if a.webClient == nil {
		a.logger.Debug("app_home_opened ignored: no web client wired")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	view := a.buildHomeView(ctx, a.cfg.IsAdminUser(ev.User))
	if _, err := a.webClient.PublishViewContext(ctx, slack.PublishViewContextRequest{
		UserID: ev.User,
		View:   view,
	}); err != nil {
		a.logger.Error("publish app home failed", "user", ev.User, "error", err)
	}
}

// buildHomeView assembles the Home-surface view. The admin additionally sees
// the control buttons (always a "Restart Murtaugh" button, plus an "Update"
// button when a newer release is available). The update check runs only for the
// admin (non-admins never trigger a GitHub lookup) and is failure-tolerant: a
// failed check renders the panel without the update button.
func (a *Gateway) buildHomeView(ctx context.Context, admin bool) slack.HomeTabViewRequest {
	version := strings.TrimSpace(a.version)
	if version == "" {
		version = "unknown"
	}
	if !admin {
		return renderHomeView(version, "", false, false)
	}
	var latest string
	var updateAvailable bool
	if a.updates != nil {
		res, err := a.updates.Check(ctx)
		if err != nil {
			a.logger.Debug("app home update check failed", "error", err)
		}
		latest, updateAvailable = res.Latest, res.Available
	}
	return renderHomeView(version, latest, updateAvailable, true)
}

// renderHomeView builds the Block Kit Home view: a "Murtaugh" header and a
// version line. When updateAvailable is set, the version line carries an
// "Update to <latest>" accessory button and a note advertising the new release.
// For the admin (admin set) a "Restart Murtaugh" button is appended so a
// graceful restart can be triggered on demand.
func renderHomeView(version, latest string, updateAvailable, admin bool) slack.HomeTabViewRequest {
	header := slack.NewHeaderBlock(
		slack.NewTextBlockObject(slack.PlainTextType, "Murtaugh", false, false),
	)

	versionText := fmt.Sprintf("*Version* `%s`", version)
	var accessory *slack.Accessory
	if updateAvailable && strings.TrimSpace(latest) != "" {
		versionText = fmt.Sprintf("*Version* `%s`\n:tada: *%s* is available", version, latest)
		button := slack.NewButtonBlockElement(
			appHomeUpdateActionID,
			latest,
			slack.NewTextBlockObject(slack.PlainTextType, fmt.Sprintf("Update to %s", latest), false, false),
		)
		button.Style = slack.StylePrimary
		accessory = slack.NewAccessory(button)
	}

	versionBlock := slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, versionText, false, false),
		nil,
		accessory,
	)
	versionBlock.BlockID = appHomeVersionBlockID

	blocks := []slack.Block{header, versionBlock}
	if admin {
		restart := slack.NewButtonBlockElement(
			appHomeRestartActionID,
			"",
			slack.NewTextBlockObject(slack.PlainTextType, "Restart Murtaugh", false, false),
		)
		restart.Style = slack.StyleDanger
		actions := slack.NewActionBlock(appHomeRestartBlockID, restart)
		blocks = append(blocks, actions)
	}

	return slack.HomeTabViewRequest{
		Type:   slack.VTHomeTab,
		Blocks: slack.Blocks{BlockSet: blocks},
	}
}

// isAppHomeUpdateClick reports whether the interaction is a click on the Home
// tab's "Update" button, so the router can open the confirmation modal before
// the workflow engine sees it.
func isAppHomeUpdateClick(interaction slack.InteractionCallback) bool {
	if interaction.Type != slack.InteractionTypeBlockActions {
		return false
	}
	for _, action := range interaction.ActionCallback.BlockActions {
		if action != nil && action.ActionID == appHomeUpdateActionID {
			return true
		}
	}
	return false
}

// isAppHomeUpdateSubmit reports whether the interaction is the submission of the
// update-confirmation modal.
func isAppHomeUpdateSubmit(interaction slack.InteractionCallback) bool {
	return interaction.Type == slack.InteractionTypeViewSubmission &&
		interaction.View.CallbackID == appHomeUpdateCallbackID
}

// appHomeUpdateTarget returns the release tag carried as the button's value.
func appHomeUpdateTarget(interaction slack.InteractionCallback) string {
	for _, action := range interaction.ActionCallback.BlockActions {
		if action != nil && action.ActionID == appHomeUpdateActionID {
			return strings.TrimSpace(action.Value)
		}
	}
	return ""
}

// handleAppHomeUpdateClick opens the confirmation modal. handleInteractive has
// already verified IsAllowedUser; this re-checks IsAdminUser since the update
// path is admin-only (the button is only ever rendered for the admin, but the
// action id could be replayed).
func (a *Gateway) handleAppHomeUpdateClick(ctx context.Context, interaction slack.InteractionCallback) {
	user := interaction.User.ID
	if !a.cfg.IsAdminUser(user) {
		a.logger.Info("denied app home update click from non-admin", "user", user)
		return
	}
	if a.webClient == nil {
		return
	}
	target := appHomeUpdateTarget(interaction)
	if _, err := a.webClient.OpenViewContext(ctx, interaction.TriggerID, a.buildUpdateModal(target)); err != nil {
		a.logger.Error("open app home update modal failed", "error", err, "target", target)
	}
}

// buildUpdateModal renders the confirm-then-update modal. The target tag rides
// in PrivateMetadata so the submit handler installs exactly what was confirmed.
func (a *Gateway) buildUpdateModal(target string) slack.ModalViewRequest {
	body := fmt.Sprintf(
		"Update to *%s* and restart Murtaugh?\n\nThe new binary is downloaded, verified, and swapped in, then the daemon restarts to run it.",
		displayTarget(target),
	)
	if a.updates != nil {
		body += fmt.Sprintf("\n\n<%s|View release notes>", a.updates.ReleaseURL(target))
	}
	return slack.ModalViewRequest{
		Type:            slack.VTModal,
		CallbackID:      appHomeUpdateCallbackID,
		PrivateMetadata: target,
		Title:           slack.NewTextBlockObject(slack.PlainTextType, "Update Murtaugh", false, false),
		Submit:          slack.NewTextBlockObject(slack.PlainTextType, "Update & restart", false, false),
		Close:           slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject(slack.MarkdownType, body, false, false),
				nil, nil,
			),
		}},
	}
}

// handleAppHomeUpdateSubmit installs the confirmed release and restarts. Slack
// has already been ack'd (closing the modal) by handleInteractive, so this runs
// on its own goroutine with a generous deadline covering the download. Progress
// and terminal status are reported to the admin's DM, since the Home tab cannot
// be updated mid-restart.
func (a *Gateway) handleAppHomeUpdateSubmit(interaction slack.InteractionCallback) {
	user := interaction.User.ID
	if !a.cfg.IsAdminUser(user) {
		a.logger.Info("denied app home update submit from non-admin", "user", user)
		return
	}
	if a.installUpdate == nil {
		a.logger.Warn("app home update submit but no installer wired")
		return
	}
	target := strings.TrimSpace(interaction.View.PrivateMetadata)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	installed, err := a.installUpdate(ctx, target)
	if err != nil {
		a.logger.Error("app home update install failed", "target", target, "error", err)
		a.notifyAdminDM(ctx, fmt.Sprintf(":warning: Update to %s failed: %v", displayTarget(target), err))
		return
	}
	if a.restart == nil {
		a.logger.Info("app home update installed but no restart coordinator wired", "version", installed)
		a.notifyAdminDM(ctx, fmt.Sprintf(":white_check_mark: Updated to %s. Restart Murtaugh to run it.", installed))
		return
	}
	a.logger.Info("app home update installed; restarting", "version", installed, "user", user)
	a.notifyAdminDM(ctx, fmt.Sprintf(":arrows_counterclockwise: Updated to %s — restarting now.", installed))
	a.restart(restartSourceInteractive, user, "", fmt.Sprintf("app home update to %s", installed))
}

// isAppHomeRestartClick reports whether the interaction is a click on the Home
// tab's "Restart Murtaugh" button, so the router can open the confirmation
// modal before the workflow engine sees it.
func isAppHomeRestartClick(interaction slack.InteractionCallback) bool {
	if interaction.Type != slack.InteractionTypeBlockActions {
		return false
	}
	for _, action := range interaction.ActionCallback.BlockActions {
		if action != nil && action.ActionID == appHomeRestartActionID {
			return true
		}
	}
	return false
}

// isAppHomeRestartSubmit reports whether the interaction is the submission of
// the restart-confirmation modal.
func isAppHomeRestartSubmit(interaction slack.InteractionCallback) bool {
	return interaction.Type == slack.InteractionTypeViewSubmission &&
		interaction.View.CallbackID == appHomeRestartCallbackID
}

// handleAppHomeRestartClick opens the restart confirmation modal. handleInteractive
// has already verified IsAllowedUser; this re-checks IsAdminUser since restart is
// admin-only (the button is only ever rendered for the admin, but the action id
// could be replayed).
func (a *Gateway) handleAppHomeRestartClick(ctx context.Context, interaction slack.InteractionCallback) {
	user := interaction.User.ID
	if !a.cfg.IsAdminUser(user) {
		a.logger.Info("denied app home restart click from non-admin", "user", user)
		return
	}
	if a.webClient == nil {
		return
	}
	if _, err := a.webClient.OpenViewContext(ctx, interaction.TriggerID, buildRestartModal()); err != nil {
		a.logger.Error("open app home restart modal failed", "error", err)
	}
}

// buildRestartModal renders the confirm-then-restart modal. It carries no
// payload (a restart has no parameters), so the submit handler keys solely on
// the callback id.
func buildRestartModal() slack.ModalViewRequest {
	body := "Restart Murtaugh now?\n\nThe daemon exits and its supervisor brings it back up; any in-flight conversations are interrupted."
	return slack.ModalViewRequest{
		Type:       slack.VTModal,
		CallbackID: appHomeRestartCallbackID,
		Title:      slack.NewTextBlockObject(slack.PlainTextType, "Restart Murtaugh", false, false),
		Submit:     slack.NewTextBlockObject(slack.PlainTextType, "Restart", false, false),
		Close:      slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false),
		Blocks: slack.Blocks{BlockSet: []slack.Block{
			slack.NewSectionBlock(
				slack.NewTextBlockObject(slack.MarkdownType, body, false, false),
				nil, nil,
			),
		}},
	}
}

// handleAppHomeRestartSubmit triggers a graceful restart from the Home tab.
// Slack has already been ack'd (closing the modal) by handleInteractive, so this
// runs on its own goroutine. Admin-gated. The Home tab cannot host the
// "restarting…" notice, so — like the slash and suggestion-confirm paths — the
// notice is posted to the admin's DM and a resume marker is persisted there
// before the coordinator is signalled, so the message flips to "back online"
// once the new process reconnects.
func (a *Gateway) handleAppHomeRestartSubmit(interaction slack.InteractionCallback) {
	user := interaction.User.ID
	if !a.cfg.IsAdminUser(user) {
		a.logger.Info("denied app home restart submit from non-admin", "user", user)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if a.restart == nil {
		a.logger.Warn("app home restart submit but no coordinator wired", "user", user)
		a.notifyAdminDM(ctx, restartSuggestionUnavailable)
		return
	}

	reason := "user requested restart from App Home"
	// Resolve the admin DM so the "restarting…" notice and its resume marker
	// land on a real channel the next startup can edit into the back-online card.
	dest, err := a.resolveSuggestionDestination(ctx, "")
	if err != nil || dest == "" {
		a.logger.Warn("app home restart: no admin DM for restart notice", "error", err)
	} else {
		noticeCtx, ncancel := context.WithTimeout(ctx, 10*time.Second)
		a.postRestartNoticeAndSaveMarker(noticeCtx, dest, "", user, restartSourceInteractive, reason)
		ncancel()
	}
	if !a.restart(restartSourceInteractive, user, dest, reason) {
		a.notifyAdminDM(ctx, restartSuggestionBusy)
		return
	}
	a.logger.Info("app home restart triggered", "user", user)
}

// notifyAdminDM posts a best-effort message to the admin's DM, reusing the same
// destination resolution as the restart-suggestion flow.
func (a *Gateway) notifyAdminDM(ctx context.Context, text string) {
	if a.messaging == nil {
		return
	}
	dest, err := a.resolveSuggestionDestination(ctx, "")
	if err != nil || dest == "" {
		return
	}
	if _, _, err := a.messaging.PostMessageContext(ctx, dest, slack.MsgOptionText(text, false)); err != nil {
		a.logger.Error("app home admin DM failed", "error", err)
	}
}

// displayTarget renders the target tag for human-facing copy, falling back to a
// neutral phrase when the click carried no tag.
func displayTarget(target string) string {
	if t := strings.TrimSpace(target); t != "" {
		return t
	}
	return "the latest release"
}
