package authcard

import (
	"encoding/json"
	"strings"
	"testing"

	slackgo "github.com/slack-go/slack"

	"github.com/miere/murtaugh/assets"
	slacklib "github.com/miere/murtaugh/internal/slack/client"
)

func testRenderer() *Renderer { return NewRenderer("", assets.FS) }

// card decodes a rendered template far enough to assert on its structure.
type card struct {
	Blocks []struct {
		Type        string                `json:"type"`
		BlockID     string                `json:"block_id"`
		Subtitle    struct{ Text string } `json:"subtitle"`
		ChildBlocks []struct {
			Type     string          `json:"type"`
			BlockID  string          `json:"block_id"`
			Elements json.RawMessage `json:"elements"`
		} `json:"child_blocks"`
	} `json:"blocks"`
}

type button struct {
	Type     string `json:"type"`
	ActionID string `json:"action_id"`
	Style    string `json:"style"`
	URL      string `json:"url"`
	Text     struct {
		Text string `json:"text"`
	} `json:"text"`
}

func render(t *testing.T, ref string, d cardData) (card, []byte) {
	t.Helper()
	raw, err := testRenderer().render(ref, d)
	if err != nil {
		t.Fatalf("render %s: %v", ref, err)
	}
	// The bytes go to Slack verbatim, so they must be valid JSON AND survive
	// the client's block decoder.
	if !json.Valid(raw) {
		t.Fatalf("render %s produced invalid JSON:\n%s", ref, raw)
	}
	if _, err := slacklib.DecodeBlocks(raw); err != nil {
		t.Fatalf("DecodeBlocks rejected %s: %v\n%s", ref, err, raw)
	}
	var c card
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("decode %s: %v", ref, err)
	}
	return c, raw
}

func baseData(state State) cardData {
	return cardData{
		ToolName:        "gcp-mcp",
		ProfileName:     "gcloud",
		URL:             "https://accounts.google.com/o/oauth2/auth?a=1",
		NeedsCode:       true,
		RequesterUserID: "U123",
		AttemptAt:       "May 14, 2026 at 3:42 PM",
		State:           string(state),
		ShowRequester:   true,
		ActionPrimary:   ActionID("corr1", ActionPrimary),
		ActionOpen:      ActionID("corr1", ActionOpen),
		ActionDeny:      ActionID("corr1", ActionDeny),
	}
}

// buttons pulls the action block's buttons out of a rendered card. Elements are
// held raw until the block type is known: a rich_text or context block also has
// "elements", but with a different element shape.
func buttons(t *testing.T, raw []byte) []button {
	t.Helper()
	var doc struct {
		Blocks []struct {
			ChildBlocks []struct {
				Type     string          `json:"type"`
				Elements json.RawMessage `json:"elements"`
			} `json:"child_blocks"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	for _, b := range doc.Blocks {
		for _, cb := range b.ChildBlocks {
			if cb.Type != "actions" {
				continue
			}
			var btns []button
			if err := json.Unmarshal(cb.Elements, &btns); err != nil {
				t.Fatalf("decode buttons: %v", err)
			}
			return btns
		}
	}
	return nil
}

func hasChildBlock(c card, blockID string) bool {
	for _, b := range c.Blocks {
		for _, cb := range b.ChildBlocks {
			if cb.BlockID == blockID {
				return true
			}
		}
	}
	return false
}

// Every state must produce valid JSON. A template that only parses in the happy
// case would fail exactly when it is reporting a failure.
func TestAllStatesRenderValidJSON(t *testing.T) {
	states := []State{StatePending, StateWorking, StateSuccess, StateDenied, StateTimeout, StateFailed}
	for _, state := range states {
		for _, needsCode := range []bool{true, false} {
			for _, showActions := range []bool{true, false} {
				for _, showFooter := range []bool{true, false} {
					d := baseData(state)
					d.NeedsCode = needsCode
					d.ShowActions = showActions
					d.ShowFooter = showFooter
					d.Reason = "something went wrong"
					render(t, AdminTemplate, d)
					render(t, RequesterTemplate, d)
				}
			}
		}
	}
}

// A code flow puts Enter Code first and primary, with Open In Browser present
// but NOT primary — the admin needs the link before they have a code.
func TestCodeFlowButtonLayout(t *testing.T) {
	d := baseData(StatePending)
	d.NeedsCode = true
	d.ShowActions = true
	_, raw := render(t, AdminTemplate, d)

	btns := buttons(t, raw)
	if len(btns) != 3 {
		t.Fatalf("expected Enter Code / Open In Browser / Deny, got %d: %+v", len(btns), btns)
	}
	if btns[0].Text.Text != "Enter Code" || btns[0].Style != "primary" {
		t.Fatalf("Enter Code should be the primary button, got %+v", btns[0])
	}
	if btns[1].Text.Text != "Open In Browser" || btns[1].Style == "primary" {
		t.Fatalf("Open In Browser must not be primary on a code flow, got %+v", btns[1])
	}
	if btns[1].URL != d.URL {
		t.Fatalf("Open In Browser URL = %q, want %q", btns[1].URL, d.URL)
	}
	// The secondary link carries its own action so clicking it can be told
	// apart from spending the single attempt.
	if btns[1].ActionID != d.ActionOpen {
		t.Fatalf("secondary action_id = %q, want %q", btns[1].ActionID, d.ActionOpen)
	}
}

// A browser-only flow drops Enter Code entirely and promotes Open In Browser.
func TestBrowserOnlyButtonLayout(t *testing.T) {
	d := baseData(StatePending)
	d.NeedsCode = false
	d.ShowActions = true
	_, raw := render(t, AdminTemplate, d)

	btns := buttons(t, raw)
	if len(btns) != 2 {
		t.Fatalf("expected Open In Browser / Deny, got %d: %+v", len(btns), btns)
	}
	if btns[0].Text.Text != "Open In Browser" || btns[0].Style != "primary" {
		t.Fatalf("Open In Browser should be primary on a browser-only flow, got %+v", btns[0])
	}
	if btns[0].URL != d.URL {
		t.Fatalf("primary URL = %q, want %q", btns[0].URL, d.URL)
	}
	for _, b := range btns {
		if b.Text.Text == "Enter Code" {
			t.Fatal("Enter Code must not appear on a browser-only flow")
		}
	}
}

// The single-attempt rule: once the primary is spent the whole bar goes.
func TestActionsBarDisappearsWhenNotShown(t *testing.T) {
	d := baseData(StateWorking)
	d.ShowActions = false
	_, raw := render(t, AdminTemplate, d)
	if btns := buttons(t, raw); len(btns) != 0 {
		t.Fatalf("expected no buttons once the attempt is spent, got %+v", btns)
	}
}

// The footer only appears after the primary has been clicked.
func TestFooterIsGatedOnShowFooter(t *testing.T) {
	d := baseData(StatePending)
	d.ShowActions = true
	d.ShowFooter = false
	c, _ := render(t, AdminTemplate, d)
	if hasChildBlock(c, "murtaugh_auth_admin_context") {
		t.Fatal("footer context shown before the primary button was clicked")
	}

	d.ShowActions = false
	d.ShowFooter = true
	c, _ = render(t, AdminTemplate, d)
	if !hasChildBlock(c, "murtaugh_auth_admin_context") {
		t.Fatal("footer context missing after the primary button was clicked")
	}
}

func TestRequesterCardAlwaysCarriesTheAttemptTime(t *testing.T) {
	c, raw := render(t, RequesterTemplate, baseData(StatePending))
	if !hasChildBlock(c, "murtaugh_auth_requester_context") {
		t.Fatal("requester card is missing its context block")
	}
	if !strings.Contains(string(raw), "May 14, 2026 at 3:42 PM") {
		t.Fatalf("attempt time missing: %s", raw)
	}
}

// The requester card is a notice, not a control surface.
func TestRequesterCardHasNoButtons(t *testing.T) {
	d := baseData(StatePending)
	d.ShowActions = true
	_, raw := render(t, RequesterTemplate, d)
	if btns := buttons(t, raw); len(btns) != 0 {
		t.Fatalf("requester card must never carry buttons, got %+v", btns)
	}
}

// The tool name is agent-supplied. A value crafted to close its JSON string and
// append siblings must not become extra blocks.
func TestToolNameCannotInjectBlocks(t *testing.T) {
	const injection = `x","block_id":"evil"},{"type":"divider"},{"type":"section","text":{"type":"mrkdwn","text":"injected`

	for _, ref := range []string{AdminTemplate, RequesterTemplate} {
		d := baseData(StatePending)
		d.ToolName = injection
		d.ShowActions = true
		c, raw := render(t, ref, d)

		if len(c.Blocks) != 1 {
			t.Fatalf("%s: expected 1 top-level block, got %d — structure was injected", ref, len(c.Blocks))
		}
		if strings.Contains(string(raw), `"type":"divider"`) {
			t.Fatalf("%s: injected block survived:\n%s", ref, raw)
		}
	}
}

// The verification URL is scraped from process output, so it is untrusted too.
func TestURLCannotInjectIntoTheButton(t *testing.T) {
	d := baseData(StatePending)
	d.ShowActions = true
	d.URL = `https://x/","style":"danger","text":{"type":"plain_text","text":"pwned`
	_, raw := render(t, AdminTemplate, d)

	btns := buttons(t, raw)
	if len(btns) != 3 {
		t.Fatalf("button count changed under injection: %+v", btns)
	}
	for _, b := range btns {
		if b.Text.Text == "pwned" {
			t.Fatalf("injected button text survived:\n%s", raw)
		}
	}
}

func TestSubtitleNamesTheTool(t *testing.T) {
	c, _ := render(t, RequesterTemplate, baseData(StatePending))
	if !strings.Contains(c.Blocks[0].Subtitle.Text, "gcp-mcp") {
		t.Fatalf("subtitle should name the tool, got %q", c.Blocks[0].Subtitle.Text)
	}
}

func TestActionIDRoundTrip(t *testing.T) {
	for _, action := range []Action{ActionPrimary, ActionOpen, ActionDeny} {
		id := ActionID("abc123", action)
		corr, got, ok := ParseActionID(id)
		if !ok {
			t.Fatalf("ParseActionID(%q) failed", id)
		}
		if corr != "abc123" || got != action {
			t.Fatalf("round-trip = (%q, %q), want (abc123, %q)", corr, got, action)
		}
	}
}

func TestParseActionIDRejectsForeignIDs(t *testing.T) {
	for _, id := range []string{"", "murtaugh_interaction:abc:0", "murtaugh_auth:", "not_ours"} {
		if _, _, ok := ParseActionID(id); ok {
			t.Fatalf("ParseActionID(%q) should not have matched", id)
		}
	}
}

func TestIsAuthInteractionMatchesOurButtons(t *testing.T) {
	ic := slackgo.InteractionCallback{
		Type: slackgo.InteractionTypeBlockActions,
		ActionCallback: slackgo.ActionCallbacks{
			BlockActions: []*slackgo.BlockAction{{ActionID: ActionID("corr9", ActionDeny)}},
		},
	}
	corr, action, ok := IsAuthInteraction(ic)
	if !ok || corr != "corr9" || action != ActionDeny {
		t.Fatalf("IsAuthInteraction = (%q, %q, %v)", corr, action, ok)
	}
}

func TestIsAuthInteractionIgnoresOtherNamespaces(t *testing.T) {
	ic := slackgo.InteractionCallback{
		Type: slackgo.InteractionTypeBlockActions,
		ActionCallback: slackgo.ActionCallbacks{
			BlockActions: []*slackgo.BlockAction{{ActionID: "murtaugh_interaction:abc:0"}},
		},
	}
	if _, _, ok := IsAuthInteraction(ic); ok {
		t.Fatal("claimed an interaction-broker click")
	}
}

func TestParseCodeSubmission(t *testing.T) {
	ic := slackgo.InteractionCallback{
		Type: slackgo.InteractionTypeViewSubmission,
		View: slackgo.View{
			CallbackID:      ModalCallbackID,
			PrivateMetadata: "corr7",
			State: &slackgo.ViewState{
				Values: map[string]map[string]slackgo.BlockAction{
					codeBlockID: {codeActionID: {Value: "  4/0AX  "}},
				},
			},
		},
	}
	corr, code, ok := ParseCodeSubmission(ic)
	if !ok || corr != "corr7" || code != "4/0AX" {
		t.Fatalf("ParseCodeSubmission = (%q, %q, %v)", corr, code, ok)
	}
}

func TestParseCodeSubmissionIgnoresOtherModals(t *testing.T) {
	ic := slackgo.InteractionCallback{
		Type: slackgo.InteractionTypeViewSubmission,
		View: slackgo.View{CallbackID: "murtaugh_interaction_form_modal"},
	}
	if _, _, ok := ParseCodeSubmission(ic); ok {
		t.Fatal("claimed the ask tool's form modal")
	}
}

func TestCodeModalCarriesCorrelation(t *testing.T) {
	m := CodeModal("corr5", "gcp-mcp")
	if m.CallbackID != ModalCallbackID {
		t.Fatalf("callback_id = %q", m.CallbackID)
	}
	if m.PrivateMetadata != "corr5" {
		t.Fatalf("private_metadata = %q", m.PrivateMetadata)
	}
}

func TestStateTerminal(t *testing.T) {
	for _, s := range []State{StateSuccess, StateDenied, StateTimeout, StateFailed} {
		if !s.Terminal() {
			t.Fatalf("%s should be terminal", s)
		}
	}
	for _, s := range []State{StatePending, StateWorking} {
		if s.Terminal() {
			t.Fatalf("%s should not be terminal", s)
		}
	}
}
