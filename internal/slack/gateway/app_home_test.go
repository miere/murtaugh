package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/updates"
	"github.com/slack-go/slack"
)

// findVersionSection returns the version section block from a rendered Home
// view, or fails the test if it is absent.
func findVersionSection(t *testing.T, view slack.HomeTabViewRequest) *slack.SectionBlock {
	t.Helper()
	for _, b := range view.Blocks.BlockSet {
		if sec, ok := b.(*slack.SectionBlock); ok && sec.BlockID == appHomeVersionBlockID {
			return sec
		}
	}
	t.Fatalf("version section %q not found in view", appHomeVersionBlockID)
	return nil
}

func TestRenderHomeView_AlwaysHasHeaderAndVersion(t *testing.T) {
	view := renderHomeView("v0.9.1", "", false, false)
	if view.Type != slack.VTHomeTab {
		t.Fatalf("expected home tab view, got %q", view.Type)
	}
	if len(view.Blocks.BlockSet) != 2 {
		t.Fatalf("expected header + version blocks, got %d", len(view.Blocks.BlockSet))
	}
	if _, ok := view.Blocks.BlockSet[0].(*slack.HeaderBlock); !ok {
		t.Fatalf("first block should be the header, got %T", view.Blocks.BlockSet[0])
	}
	sec := findVersionSection(t, view)
	if !strings.Contains(sec.Text.Text, "v0.9.1") {
		t.Fatalf("version text missing the version: %q", sec.Text.Text)
	}
}

func TestRenderHomeView_NoButtonWithoutUpdate(t *testing.T) {
	sec := findVersionSection(t, renderHomeView("v0.9.1", "", false, false))
	if sec.Accessory != nil {
		t.Fatalf("no update ⇒ no accessory button, got %+v", sec.Accessory)
	}
}

func TestRenderHomeView_ButtonWhenUpdateAvailable(t *testing.T) {
	view := renderHomeView("v0.9.1", "v0.9.4", true, false)
	sec := findVersionSection(t, view)
	if sec.Accessory == nil || sec.Accessory.ButtonElement == nil {
		t.Fatalf("expected an Update accessory button, got %+v", sec.Accessory)
	}
	btn := sec.Accessory.ButtonElement
	if btn.ActionID != appHomeUpdateActionID {
		t.Fatalf("button action id = %q, want %q", btn.ActionID, appHomeUpdateActionID)
	}
	if btn.Value != "v0.9.4" {
		t.Fatalf("button value (target tag) = %q, want v0.9.4", btn.Value)
	}
	if !strings.Contains(sec.Text.Text, "v0.9.4") {
		t.Fatalf("version text should advertise the new release: %q", sec.Text.Text)
	}
}

func TestRenderHomeView_NoButtonWhenLatestMissing(t *testing.T) {
	// Defensive: available=true but no tag ⇒ still no button (nothing to target).
	sec := findVersionSection(t, renderHomeView("v0.9.1", "", true, false))
	if sec.Accessory != nil {
		t.Fatalf("missing tag ⇒ no button, got %+v", sec.Accessory)
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
	sec := findVersionSection(t, gw.buildHomeView(context.Background(), true))
	if sec.Accessory == nil {
		t.Fatal("admin with an available update should see the button")
	}
}

func TestBuildHomeView_NonAdminNeverSeesButton(t *testing.T) {
	gw := newGatewayForHome("UADMIN00", "v0.9.1", stubChecker("v0.9.1", "v0.9.4"))
	sec := findVersionSection(t, gw.buildHomeView(context.Background(), false))
	if sec.Accessory != nil {
		t.Fatal("non-admin must never see the update button")
	}
}

func TestBuildHomeView_UnknownVersionWhenBlank(t *testing.T) {
	gw := newGatewayForHome("UADMIN00", "", nil)
	sec := findVersionSection(t, gw.buildHomeView(context.Background(), true))
	if !strings.Contains(sec.Text.Text, "unknown") {
		t.Fatalf("blank version should render as unknown: %q", sec.Text.Text)
	}
}

func TestBuildHomeView_DevBuildNoButton(t *testing.T) {
	// "dev" is not a release ⇒ the checker short-circuits, no button even for admin.
	gw := newGatewayForHome("UADMIN00", "dev", stubChecker("dev", "v9.9.9"))
	sec := findVersionSection(t, gw.buildHomeView(context.Background(), true))
	if sec.Accessory != nil {
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

// findRestartButton returns the "Restart Murtaugh" button from the actions
// block of a rendered Home view, or nil when no such block/button is present.
func findRestartButton(view slack.HomeTabViewRequest) *slack.ButtonBlockElement {
	for _, b := range view.Blocks.BlockSet {
		act, ok := b.(*slack.ActionBlock)
		if !ok || act.BlockID != appHomeRestartBlockID {
			continue
		}
		for _, el := range act.Elements.ElementSet {
			if btn, ok := el.(*slack.ButtonBlockElement); ok && btn.ActionID == appHomeRestartActionID {
				return btn
			}
		}
	}
	return nil
}

func TestRenderHomeView_AdminSeesRestartButton(t *testing.T) {
	btn := findRestartButton(renderHomeView("v0.9.1", "", false, true))
	if btn == nil {
		t.Fatal("admin should always see the Restart Murtaugh button")
	}
	if btn.Style != slack.StyleDanger {
		t.Fatalf("restart button style = %q, want danger", btn.Style)
	}
}

func TestRenderHomeView_NonAdminNoRestartButton(t *testing.T) {
	if btn := findRestartButton(renderHomeView("v0.9.1", "", false, false)); btn != nil {
		t.Fatal("non-admin must never see the Restart Murtaugh button")
	}
}

func TestBuildHomeView_AdminSeesRestartButtonWithoutUpdateChecker(t *testing.T) {
	// No update checker wired ⇒ no Update button, but the restart button must
	// still be offered to the admin (it is independent of the update path).
	gw := newGatewayForHome("UADMIN00", "v0.9.1", nil)
	view := gw.buildHomeView(context.Background(), true)
	if btn := findRestartButton(view); btn == nil {
		t.Fatal("admin should see the restart button even with no update checker")
	}
	if sec := findVersionSection(t, view); sec.Accessory != nil {
		t.Fatal("no update checker ⇒ no update accessory button")
	}
}

func TestBuildHomeView_NonAdminNeverSeesRestartButton(t *testing.T) {
	gw := newGatewayForHome("UADMIN00", "v0.9.1", stubChecker("v0.9.1", "v0.9.4"))
	if btn := findRestartButton(gw.buildHomeView(context.Background(), false)); btn != nil {
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
