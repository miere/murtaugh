package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/updates"
	"github.com/slack-go/slack"
)

// findVersionContext returns the version context block from a rendered Home
// view, or fails the test if it is absent.
func findVersionContext(t *testing.T, view slack.HomeTabViewRequest) *slack.ContextBlock {
	t.Helper()
	for _, b := range view.Blocks.BlockSet {
		if ctxBlock, ok := b.(*slack.ContextBlock); ok && ctxBlock.BlockID == appHomeVersionBlockID {
			return ctxBlock
		}
	}
	t.Fatalf("version context %q not found in view", appHomeVersionBlockID)
	return nil
}

// versionText flattens the version context block into its rendered text.
func versionText(t *testing.T, view slack.HomeTabViewRequest) string {
	t.Helper()
	var parts []string
	for _, el := range findVersionContext(t, view).ContextElements.Elements {
		if txt, ok := el.(*slack.TextBlockObject); ok {
			parts = append(parts, txt.Text)
		}
	}
	return strings.Join(parts, " ")
}

// findBanner returns the banner image block, or nil when the view carries none.
func findBanner(view slack.HomeTabViewRequest) *slack.ImageBlock {
	for _, b := range view.Blocks.BlockSet {
		if img, ok := b.(*slack.ImageBlock); ok && img.BlockID == appHomeBannerBlockID {
			return img
		}
	}
	return nil
}

// findHomeButton returns the button carrying actionID from the Home view's
// control row, or nil when the row (or the button) is absent.
func findHomeButton(view slack.HomeTabViewRequest, actionID string) *slack.ButtonBlockElement {
	for _, b := range view.Blocks.BlockSet {
		act, ok := b.(*slack.ActionBlock)
		if !ok || act.BlockID != appHomeActionsBlockID {
			continue
		}
		for _, el := range act.Elements.ElementSet {
			if btn, ok := el.(*slack.ButtonBlockElement); ok && btn.ActionID == actionID {
				return btn
			}
		}
	}
	return nil
}

func hasDivider(view slack.HomeTabViewRequest) bool {
	for _, b := range view.Blocks.BlockSet {
		if _, ok := b.(*slack.DividerBlock); ok {
			return true
		}
	}
	return false
}

func TestRenderHomeView_AlwaysHasBannerAndVersion(t *testing.T) {
	view := renderHomeView("v0.9.1", "", false, false)
	if view.Type != slack.VTHomeTab {
		t.Fatalf("expected home tab view, got %q", view.Type)
	}
	if len(view.Blocks.BlockSet) != 2 {
		t.Fatalf("expected banner + version blocks, got %d", len(view.Blocks.BlockSet))
	}
	banner, ok := view.Blocks.BlockSet[0].(*slack.ImageBlock)
	if !ok {
		t.Fatalf("first block should be the banner image, got %T", view.Blocks.BlockSet[0])
	}
	if banner.ImageURL != appHomeBannerURL {
		t.Fatalf("banner url = %q, want %q", banner.ImageURL, appHomeBannerURL)
	}
	if strings.TrimSpace(banner.AltText) == "" {
		t.Fatal("banner must carry alt text")
	}
	if got := versionText(t, view); got != "Version: v0.9.1" {
		t.Fatalf("version line = %q, want %q", got, "Version: v0.9.1")
	}
}

func TestRenderHomeView_NonAdminHasNoControls(t *testing.T) {
	// Even with an update available, a non-admin gets no divider and no buttons.
	view := renderHomeView("v0.9.1", "v0.9.4", true, false)
	if hasDivider(view) {
		t.Fatal("non-admin view must not carry the controls divider")
	}
	if btn := findHomeButton(view, appHomeUpdateActionID); btn != nil {
		t.Fatal("non-admin must never see the upgrade button")
	}
	if btn := findHomeButton(view, appHomeRestartActionID); btn != nil {
		t.Fatal("non-admin must never see the restart button")
	}
}

func TestRenderHomeView_AdminSeesRestartButton(t *testing.T) {
	view := renderHomeView("v0.9.1", "", false, true)
	if !hasDivider(view) {
		t.Fatal("admin view should separate the controls with a divider")
	}
	btn := findHomeButton(view, appHomeRestartActionID)
	if btn == nil {
		t.Fatal("admin should always see the Restart button")
	}
	if btn.Text.Text != "Restart" {
		t.Fatalf("restart button label = %q, want %q", btn.Text.Text, "Restart")
	}
	if btn.Style != "" {
		t.Fatalf("restart button style = %q, want the default style", btn.Style)
	}
}

func TestRenderHomeView_NoUpgradeButtonWithoutUpdate(t *testing.T) {
	if btn := findHomeButton(renderHomeView("v0.9.1", "", false, true), appHomeUpdateActionID); btn != nil {
		t.Fatalf("no update ⇒ no upgrade button, got %+v", btn)
	}
}

func TestRenderHomeView_UpgradeButtonWhenUpdateAvailable(t *testing.T) {
	view := renderHomeView("v0.9.1", "v0.9.4", true, true)
	btn := findHomeButton(view, appHomeUpdateActionID)
	if btn == nil {
		t.Fatal("expected an Upgrade button when a release is available")
	}
	if btn.Value != "v0.9.4" {
		t.Fatalf("button value (target tag) = %q, want v0.9.4", btn.Value)
	}
	if btn.Text.Text != "Upgrade to version v0.9.4" {
		t.Fatalf("button label = %q, want %q", btn.Text.Text, "Upgrade to version v0.9.4")
	}
	if btn.Style != slack.StyleDanger {
		t.Fatalf("upgrade button style = %q, want danger", btn.Style)
	}
	// The version line stays a plain statement of what is running; the button
	// is what advertises the new release.
	if got := versionText(t, view); got != "Version: v0.9.1" {
		t.Fatalf("version line = %q, want %q", got, "Version: v0.9.1")
	}
}

func TestRenderHomeView_UpgradeLeadsTheControlRow(t *testing.T) {
	view := renderHomeView("v0.9.1", "v0.9.4", true, true)
	var actions *slack.ActionBlock
	for _, b := range view.Blocks.BlockSet {
		if act, ok := b.(*slack.ActionBlock); ok && act.BlockID == appHomeActionsBlockID {
			actions = act
		}
	}
	if actions == nil {
		t.Fatal("admin view should carry a controls row")
	}
	if len(actions.Elements.ElementSet) != 2 {
		t.Fatalf("expected upgrade + restart buttons, got %d", len(actions.Elements.ElementSet))
	}
	first, ok := actions.Elements.ElementSet[0].(*slack.ButtonBlockElement)
	if !ok || first.ActionID != appHomeUpdateActionID {
		t.Fatalf("upgrade should lead the control row, got %+v", actions.Elements.ElementSet[0])
	}
}

func TestRenderHomeView_NoUpgradeWhenLatestMissing(t *testing.T) {
	// Defensive: available=true but no tag ⇒ still no button (nothing to target).
	if btn := findHomeButton(renderHomeView("v0.9.1", "", true, true), appHomeUpdateActionID); btn != nil {
		t.Fatalf("missing tag ⇒ no button, got %+v", btn)
	}
}

func TestHomeViewWithoutBanner_DropsOnlyTheImage(t *testing.T) {
	view := renderHomeView("v0.9.1", "v0.9.4", true, true)
	stripped, dropped := homeViewWithoutBanner(view)
	if !dropped {
		t.Fatal("expected the banner to be dropped")
	}
	if findBanner(stripped) != nil {
		t.Fatal("stripped view must not carry the banner")
	}
	if len(stripped.Blocks.BlockSet) != len(view.Blocks.BlockSet)-1 {
		t.Fatalf("expected exactly one block dropped, got %d → %d",
			len(view.Blocks.BlockSet), len(stripped.Blocks.BlockSet))
	}
	// The controls — the reason for the retry — must survive.
	if findHomeButton(stripped, appHomeRestartActionID) == nil {
		t.Fatal("stripped view must keep the restart button")
	}
	if findHomeButton(stripped, appHomeUpdateActionID) == nil {
		t.Fatal("stripped view must keep the upgrade button")
	}
	versionText(t, stripped)
}

func TestHomeViewWithoutBanner_NoBannerToDrop(t *testing.T) {
	view := slack.HomeTabViewRequest{
		Type:   slack.VTHomeTab,
		Blocks: slack.Blocks{BlockSet: []slack.Block{slack.NewDividerBlock()}},
	}
	if _, dropped := homeViewWithoutBanner(view); dropped {
		t.Fatal("a view with no banner has nothing to drop")
	}
}

func newGatewayForHome(admin string, version string, checker *updates.Checker) *Gateway {
	return &Gateway{
		logger:  newSilentLogger(),
		cfg:     config.AccessConfig{AdminUser: admin},
		version: version,
		updates: checker,
	}
}

func stubChecker(current, latest string) *updates.Checker {
	return updates.New(updates.Deps{
		Current: current,
		Owner:   "miere",
		Repo:    "murtaugh",
		HTTPGet: func(context.Context, string) ([]byte, error) {
			return []byte(`{"tag_name":"` + latest + `"}`), nil
		},
	})
}

func TestBuildHomeView_AdminSeesButtonOnUpdate(t *testing.T) {
	gw := newGatewayForHome("UADMIN00", "v0.9.1", stubChecker("v0.9.1", "v0.9.4"))
	if btn := findHomeButton(gw.buildHomeView(context.Background(), true), appHomeUpdateActionID); btn == nil {
		t.Fatal("admin with an available update should see the button")
	}
}

func TestBuildHomeView_NonAdminNeverSeesButton(t *testing.T) {
	gw := newGatewayForHome("UADMIN00", "v0.9.1", stubChecker("v0.9.1", "v0.9.4"))
	if btn := findHomeButton(gw.buildHomeView(context.Background(), false), appHomeUpdateActionID); btn != nil {
		t.Fatal("non-admin must never see the update button")
	}
}

func TestBuildHomeView_UnknownVersionWhenBlank(t *testing.T) {
	gw := newGatewayForHome("UADMIN00", "", nil)
	if got := versionText(t, gw.buildHomeView(context.Background(), true)); !strings.Contains(got, "unknown") {
		t.Fatalf("blank version should render as unknown: %q", got)
	}
}

func TestBuildHomeView_DevBuildNoButton(t *testing.T) {
	// "dev" is not a release ⇒ the checker short-circuits, no button even for admin.
	gw := newGatewayForHome("UADMIN00", "dev", stubChecker("dev", "v9.9.9"))
	if btn := findHomeButton(gw.buildHomeView(context.Background(), true), appHomeUpdateActionID); btn != nil {
		t.Fatal("a dev build must not offer an update")
	}
}

func updateClick(user, target string) slack.InteractionCallback {
	return slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
		User: slack.User{ID: user},
		ActionCallback: slack.ActionCallbacks{BlockActions: []*slack.BlockAction{{
			ActionID: appHomeUpdateActionID,
			Value:    target,
		}}},
	}
}

func TestIsAppHomeUpdateClick(t *testing.T) {
	if !isAppHomeUpdateClick(updateClick("U1", "v0.9.4")) {
		t.Fatal("expected the Update button click to be recognised")
	}
	// A different block action must not match.
	other := updateClick("U1", "v0.9.4")
	other.ActionCallback.BlockActions[0].ActionID = "something_else"
	if isAppHomeUpdateClick(other) {
		t.Fatal("unrelated action id must not match")
	}
	// A view_submission is not a click.
	if isAppHomeUpdateClick(slack.InteractionCallback{Type: slack.InteractionTypeViewSubmission}) {
		t.Fatal("view_submission is not a block-action click")
	}
}

func TestAppHomeUpdateTarget(t *testing.T) {
	if got := appHomeUpdateTarget(updateClick("U1", " v0.9.4 ")); got != "v0.9.4" {
		t.Fatalf("target = %q, want trimmed v0.9.4", got)
	}
}

func TestIsAppHomeUpdateSubmit(t *testing.T) {
	submit := slack.InteractionCallback{
		Type: slack.InteractionTypeViewSubmission,
		View: slack.View{CallbackID: appHomeUpdateCallbackID},
	}
	if !isAppHomeUpdateSubmit(submit) {
		t.Fatal("expected the confirm-modal submission to be recognised")
	}
	// Another modal's submission must not match.
	other := submit
	other.View.CallbackID = "ask_form"
	if isAppHomeUpdateSubmit(other) {
		t.Fatal("a different modal callback id must not match")
	}
}

func TestBuildUpdateModal_CarriesTargetAndCallback(t *testing.T) {
	gw := newGatewayForHome("UADMIN00", "v0.9.1", stubChecker("v0.9.1", "v0.9.4"))
	modal := gw.buildUpdateModal("v0.9.4")
	if modal.CallbackID != appHomeUpdateCallbackID {
		t.Fatalf("callback id = %q, want %q", modal.CallbackID, appHomeUpdateCallbackID)
	}
	if modal.PrivateMetadata != "v0.9.4" {
		t.Fatalf("private metadata (target) = %q, want v0.9.4", modal.PrivateMetadata)
	}
	body := modal.Blocks.BlockSet[0].(*slack.SectionBlock).Text.Text
	if !strings.Contains(body, "v0.9.4") {
		t.Fatalf("modal body should name the target: %q", body)
	}
	if !strings.Contains(body, "releases/tag/v0.9.4") {
		t.Fatalf("modal body should link the release notes: %q", body)
	}
}

func TestBuildHomeView_AdminSeesRestartButtonWithoutUpdateChecker(t *testing.T) {
	// No update checker wired ⇒ no Upgrade button, but the restart button must
	// still be offered to the admin (it is independent of the update path).
	gw := newGatewayForHome("UADMIN00", "v0.9.1", nil)
	view := gw.buildHomeView(context.Background(), true)
	if btn := findHomeButton(view, appHomeRestartActionID); btn == nil {
		t.Fatal("admin should see the restart button even with no update checker")
	}
	if btn := findHomeButton(view, appHomeUpdateActionID); btn != nil {
		t.Fatal("no update checker ⇒ no upgrade button")
	}
}

func TestBuildHomeView_NonAdminNeverSeesRestartButton(t *testing.T) {
	gw := newGatewayForHome("UADMIN00", "v0.9.1", stubChecker("v0.9.1", "v0.9.4"))
	if btn := findHomeButton(gw.buildHomeView(context.Background(), false), appHomeRestartActionID); btn != nil {
		t.Fatal("non-admin must never see the restart button")
	}
}

func restartClick(user string) slack.InteractionCallback {
	return slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
		User: slack.User{ID: user},
		ActionCallback: slack.ActionCallbacks{BlockActions: []*slack.BlockAction{{
			ActionID: appHomeRestartActionID,
		}}},
	}
}

func TestIsAppHomeRestartClick(t *testing.T) {
	if !isAppHomeRestartClick(restartClick("U1")) {
		t.Fatal("expected the Restart button click to be recognised")
	}
	other := restartClick("U1")
	other.ActionCallback.BlockActions[0].ActionID = appHomeUpdateActionID
	if isAppHomeRestartClick(other) {
		t.Fatal("the Update button click must not match the restart predicate")
	}
	if isAppHomeRestartClick(slack.InteractionCallback{Type: slack.InteractionTypeViewSubmission}) {
		t.Fatal("view_submission is not a block-action click")
	}
}

func TestIsAppHomeRestartSubmit(t *testing.T) {
	submit := slack.InteractionCallback{
		Type: slack.InteractionTypeViewSubmission,
		View: slack.View{CallbackID: appHomeRestartCallbackID},
	}
	if !isAppHomeRestartSubmit(submit) {
		t.Fatal("expected the restart-confirm modal submission to be recognised")
	}
	other := submit
	other.View.CallbackID = appHomeUpdateCallbackID
	if isAppHomeRestartSubmit(other) {
		t.Fatal("the update modal callback id must not match the restart predicate")
	}
}

func TestBuildRestartModal_CarriesCallback(t *testing.T) {
	modal := buildRestartModal()
	if modal.CallbackID != appHomeRestartCallbackID {
		t.Fatalf("callback id = %q, want %q", modal.CallbackID, appHomeRestartCallbackID)
	}
	if modal.PrivateMetadata != "" {
		t.Fatalf("restart modal should carry no payload, got metadata %q", modal.PrivateMetadata)
	}
}
