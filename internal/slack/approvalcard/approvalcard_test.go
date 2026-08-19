package approvalcard

import (
	"encoding/json"
	"strings"
	"testing"

	slackgo "github.com/slack-go/slack"

	"github.com/miere/murtaugh/assets"
	slacklib "github.com/miere/murtaugh/internal/slack/client"
)

func testRenderer() *Renderer { return NewRenderer("", assets.FS) }

// actionsBlockID stands in for the broker's routing constant, which the caller
// supplies so this package never has to know it.
const actionsBlockID = "murtaugh_interaction"

// card decodes a rendered template far enough to assert on its structure.
type card struct {
	Blocks []struct {
		Type            string `json:"type"`
		BlockID         string `json:"block_id"`
		IsCollapsible   bool   `json:"is_collapsible"`
		DefaultCollapse bool   `json:"default_collapsed"`
		Title           struct {
			Text string `json:"text"`
		} `json:"title"`
		Subtitle struct {
			Text string `json:"text"`
		} `json:"subtitle"`
		Icon struct {
			ImageURL string `json:"image_url"`
		} `json:"icon"`
		ChildBlocks []struct {
			Type     string          `json:"type"`
			BlockID  string          `json:"block_id"`
			Elements json.RawMessage `json:"elements"`
		} `json:"child_blocks"`
	} `json:"blocks"`
}

type button struct {
	ActionID string `json:"action_id"`
	Value    string `json:"value"`
	Style    string `json:"style"`
	Text     struct {
		Text string `json:"text"`
	} `json:"text"`
}

// pending renders a pending card and runs every universal assertion over it.
func pending(t *testing.T, spec Spec, opts []Option) (card, []byte) {
	t.Helper()
	raw, err := testRenderer().Pending(spec, actionsBlockID, opts)
	return check(t, raw, err), raw
}

// resolved renders a settled card and runs every universal assertion over it.
func resolved(t *testing.T, spec Spec, outcome Outcome, decidedBy string) (card, []byte) {
	t.Helper()
	raw, err := testRenderer().Resolved(spec, outcome, decidedBy)
	return check(t, raw, err), raw
}

// check runs the assertions that apply to every rendered card: the bytes go to
// Slack verbatim, so they must be valid JSON, must survive the client's block
// decoder, and must come back out of it unchanged.
func check(t *testing.T, raw []byte, err error) card {
	t.Helper()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("render produced invalid JSON:\n%s", raw)
	}
	decoded, err := slacklib.DecodeBlocks(raw)
	if err != nil {
		t.Fatalf("DecodeBlocks rejected the card: %v\n%s", err, raw)
	}
	// The whole reason these cards are templates: a container/rich_text payload
	// run through slack-go's typed builders loses undeclared fields silently.
	// Re-marshalling must reproduce the block array exactly.
	remarshalled, err := json.Marshal(slackgo.Blocks{BlockSet: decoded})
	if err != nil {
		t.Fatalf("re-marshal decoded blocks: %v", err)
	}
	var want struct {
		Blocks json.RawMessage `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("decode rendered card: %v", err)
	}
	if !jsonEqual(t, remarshalled, want.Blocks) {
		t.Fatalf("blocks changed passing through DecodeBlocks:\n got: %s\nwant: %s", remarshalled, want.Blocks)
	}
	var c card
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if len(c.Blocks) != 1 {
		t.Fatalf("want exactly one top-level block, got %d", len(c.Blocks))
	}
	if c.Blocks[0].Type != "container" {
		t.Fatalf("want a container block, got %q", c.Blocks[0].Type)
	}
	if len(c.Blocks[0].ChildBlocks) == 0 {
		t.Fatal("container rendered with no child_blocks; Slack rejects an empty container")
	}
	return c
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("unmarshal a: %v", err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("unmarshal b: %v", err)
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return string(ja) == string(jb)
}

func testSpec() Spec {
	return Spec{
		ToolName: "terminal",
		Detail:   "grep -n \"perm\" internal/agent/claudecode/claudecode_test.go |\n\thead -20",
		Language: "bash",
	}
}

func testOptions() []Option {
	return []Option{
		{ActionID: "murtaugh_interaction:corr1:0", Value: `{"id":"approve","label":"Approve"}`, Label: "Approve", Style: "primary"},
		{ActionID: "murtaugh_interaction:corr1:1", Value: `{"id":"deny","label":"Deny"}`, Label: "Deny", Style: "danger"},
	}
}

func buttonsOf(t *testing.T, c card) []button {
	t.Helper()
	for _, cb := range c.Blocks[0].ChildBlocks {
		if cb.Type != "actions" {
			continue
		}
		var btns []button
		if err := json.Unmarshal(cb.Elements, &btns); err != nil {
			t.Fatalf("decode buttons: %v", err)
		}
		return btns
	}
	return nil
}

func footerOf(t *testing.T, c card) string {
	t.Helper()
	for _, cb := range c.Blocks[0].ChildBlocks {
		if cb.Type != "context" {
			continue
		}
		var elems []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(cb.Elements, &elems); err != nil {
			t.Fatalf("decode context: %v", err)
		}
		if len(elems) > 0 {
			return elems[0].Text
		}
	}
	return ""
}

// The pending card must carry the broker's action_id and value through
// untouched: they are the only way a click on a button nested inside a
// container's child_blocks gets correlated back to the blocked tool call.
func TestPendingCarriesBrokerAddressing(t *testing.T) {
	c, _ := pending(t, testSpec(), testOptions())

	var got string
	for _, cb := range c.Blocks[0].ChildBlocks {
		if cb.Type == "actions" {
			got = cb.BlockID
		}
	}
	if got != actionsBlockID {
		t.Fatalf("actions block_id = %q, want the caller's routing constant %q", got, actionsBlockID)
	}

	btns := buttonsOf(t, c)
	if len(btns) != 2 {
		t.Fatalf("want 2 buttons, got %d", len(btns))
	}
	for i, want := range testOptions() {
		if btns[i].ActionID != want.ActionID {
			t.Errorf("button %d action_id = %q, want %q", i, btns[i].ActionID, want.ActionID)
		}
		if btns[i].Value != want.Value {
			t.Errorf("button %d value = %q, want %q", i, btns[i].Value, want.Value)
		}
		if btns[i].Style != want.Style {
			t.Errorf("button %d style = %q, want %q", i, btns[i].Style, want.Style)
		}
		if btns[i].Text.Text != want.Label {
			t.Errorf("button %d label = %q, want %q", i, btns[i].Text.Text, want.Label)
		}
	}
	if c.Blocks[0].IsCollapsible {
		t.Error("a card waiting on a decision must not be collapsible")
	}
}

// An ACP agent declares its own options, and there can be four of them rather
// than the mock's two. The template must render whatever it is handed.
func TestPendingRendersAnyNumberOfOptions(t *testing.T) {
	opts := []Option{
		{ActionID: "a:0", Value: `{"id":"allow_once"}`, Label: "Allow once", Style: "primary"},
		{ActionID: "a:1", Value: `{"id":"allow_always"}`, Label: "Allow always", Style: "primary"},
		{ActionID: "a:2", Value: `{"id":"reject_once"}`, Label: "Reject once", Style: "danger"},
		{ActionID: "a:3", Value: `{"id":"reject_always"}`, Label: "Reject always", Style: "danger"},
	}
	c, _ := pending(t, Spec{ToolName: "edit"}, opts)
	if got := len(buttonsOf(t, c)); got != len(opts) {
		t.Fatalf("rendered %d buttons, want %d", got, len(opts))
	}
}

// A neutral option carries no style key at all. Slack rejects "style": "" —
// omitting it is the only valid way to render an unstyled button.
func TestPendingOmitsEmptyStyle(t *testing.T) {
	c, raw := pending(t, Spec{ToolName: "edit"}, []Option{{ActionID: "a:0", Value: `{"id":"x"}`, Label: "Something"}})
	if got := buttonsOf(t, c)[0].Style; got != "" {
		t.Fatalf("style = %q, want it omitted", got)
	}
	if strings.Contains(string(raw), `"style"`) {
		t.Fatalf("card emitted a style key for an unstyled button:\n%s", raw)
	}
}

// A tool with no command to show must not render an empty code block.
func TestPendingWithoutDetailRendersNoCodeBlock(t *testing.T) {
	c, _ := pending(t, Spec{ToolName: "edit"}, testOptions())
	for _, cb := range c.Blocks[0].ChildBlocks {
		if cb.Type == "rich_text" {
			t.Fatal("rendered a code block for a spec with no detail")
		}
	}
}

// Every outcome must render. A template that only parsed in the happy case would
// break exactly when it was reporting a refusal.
func TestResolvedRendersEveryOutcome(t *testing.T) {
	for _, tc := range []struct {
		outcome    Outcome
		decidedBy  string
		wantTitle  string
		wantFooter string
	}{
		{OutcomeApproved, "U123", "Approved", "<@U123>"},
		{OutcomeDenied, "U123", "Denied", "<@U123>"},
		{OutcomeTimedOut, "", "Approval Timed Out", "No response in time"},
		{OutcomeDismissed, "", "Approval Dismissed", "Dismissed"},
	} {
		t.Run(string(tc.outcome), func(t *testing.T) {
			c, _ := resolved(t, testSpec(), tc.outcome, tc.decidedBy)
			if c.Blocks[0].Title.Text != tc.wantTitle {
				t.Errorf("title = %q, want %q", c.Blocks[0].Title.Text, tc.wantTitle)
			}
			if !c.Blocks[0].IsCollapsible || !c.Blocks[0].DefaultCollapse {
				t.Error("a settled card must be collapsible and collapsed by default")
			}
			if footer := footerOf(t, c); !strings.Contains(footer, tc.wantFooter) {
				t.Errorf("footer = %q, want it to contain %q", footer, tc.wantFooter)
			}
		})
	}
}

// A settled card carries no buttons: the decision is made, and a live button on
// it would resolve nothing.
func TestResolvedHasNoButtons(t *testing.T) {
	c, _ := resolved(t, testSpec(), OutcomeApproved, "U1")
	if btns := buttonsOf(t, c); len(btns) != 0 {
		t.Fatalf("settled card rendered %d buttons", len(btns))
	}
}

// The tool name and the decider are the two things a reader of the thread needs
// from a settled card. Losing the decider was the regression the canvas mock
// would have shipped.
func TestResolvedNamesToolAndDecider(t *testing.T) {
	c, _ := resolved(t, testSpec(), OutcomeApproved, "U9")
	if !strings.Contains(c.Blocks[0].Subtitle.Text, "'terminal'") {
		t.Errorf("subtitle = %q, want it to name the tool", c.Blocks[0].Subtitle.Text)
	}
	if footer := footerOf(t, c); !strings.Contains(footer, "<@U9>") {
		t.Errorf("footer = %q, want it to name the decider", footer)
	}
}

// A timeout is not a refusal: nobody decided, so the card must not name one.
func TestResolvedWithoutDeciderNamesNobody(t *testing.T) {
	c, _ := resolved(t, testSpec(), OutcomeTimedOut, "")
	if footer := footerOf(t, c); strings.Contains(footer, "<@") {
		t.Errorf("footer = %q, want no decider mention", footer)
	}
}

// An outcome where the tool did not run must be visually distinct from one where
// it did, or the collapsed card is unreadable at a glance.
func TestResolvedIconDistinguishesRefusal(t *testing.T) {
	approved, _ := resolved(t, testSpec(), OutcomeApproved, "U1")
	denied, _ := resolved(t, testSpec(), OutcomeDenied, "U1")
	if approved.Blocks[0].Icon.ImageURL == denied.Blocks[0].Icon.ImageURL {
		t.Fatal("approved and denied cards render the same icon")
	}
}

// The command line is agent-supplied, so it is untrusted. text/template does no
// escaping: a value crafted to close its JSON string and open sibling keys would
// produce *valid* JSON with attacker-chosen structure, which no validity check
// catches. Every interpolation goes through json/jsonstr, so the payload must
// land as one inert string.
func TestDetailCannotInjectBlocks(t *testing.T) {
	evil := `x"}]},{"type":"actions","block_id":"pwned","elements":[{"type":"button","action_id":"evil","text":{"type":"plain_text","text":"Click"}}]},{"a":"`
	c, _ := pending(t, Spec{ToolName: "terminal", Detail: evil, Language: "bash"}, testOptions())

	// The detail block plus the actions block, and nothing the payload smuggled in.
	if len(c.Blocks) != 1 {
		t.Fatalf("injection created %d top-level blocks", len(c.Blocks))
	}
	if len(c.Blocks[0].ChildBlocks) != 2 {
		t.Fatalf("injection created %d child blocks, want 2", len(c.Blocks[0].ChildBlocks))
	}
	for _, cb := range c.Blocks[0].ChildBlocks {
		if cb.BlockID == "pwned" {
			t.Fatal("injected block survived into the card")
		}
	}
	if btns := buttonsOf(t, c); len(btns) != 2 {
		t.Fatalf("injection changed the button set to %d buttons", len(btns))
	}
}

// The same payload must be inert on the settled card, which renders the detail
// through a second template.
func TestDetailCannotInjectIntoResolved(t *testing.T) {
	evil := `x"}]},{"type":"actions","block_id":"pwned","elements":[]},{"a":"`
	c, _ := resolved(t, Spec{ToolName: "terminal", Detail: evil}, OutcomeApproved, "U1")
	for _, cb := range c.Blocks[0].ChildBlocks {
		if cb.BlockID == "pwned" {
			t.Fatal("injected block survived into the settled card")
		}
	}
}

// The language hint is optional; an empty one must not emit an empty key.
func TestLanguageOmittedWhenUnset(t *testing.T) {
	_, raw := pending(t, Spec{ToolName: "edit", Detail: "some detail"}, testOptions())
	if strings.Contains(string(raw), `"language"`) {
		t.Fatalf("emitted a language key with no hint set:\n%s", raw)
	}
}

// A missing tool name must still read as a sentence rather than empty quotes.
func TestUnknownToolDegradesGracefully(t *testing.T) {
	c, _ := pending(t, Spec{}, testOptions())
	if strings.Contains(c.Blocks[0].Subtitle.Text, "''") {
		t.Fatalf("subtitle = %q, want no empty quotes", c.Blocks[0].Subtitle.Text)
	}
	if got := FallbackText(Spec{}); got == "" {
		t.Fatal("FallbackText(empty) rendered nothing")
	}
}

// The notification line names the tool but never the command: a push
// notification is the least private surface Slack has.
func TestFallbackTextOmitsTheCommand(t *testing.T) {
	got := FallbackText(testSpec())
	if !strings.Contains(got, "terminal") {
		t.Errorf("FallbackText = %q, want it to name the tool", got)
	}
	if strings.Contains(got, "grep") {
		t.Errorf("FallbackText = %q, want it to omit the command", got)
	}
}
