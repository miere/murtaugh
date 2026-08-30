package configcard

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/miere/murtaugh/assets"
)

func renderer(t *testing.T) *Renderer {
	t.Helper()
	return NewRenderer(t.TempDir(), assets.FS)
}

// decodeBlocks parses the rendered card, failing the test on malformed JSON —
// the failure mode a template edit produces, and one Slack reports only as a
// generic invalid_blocks.
func decodeBlocks(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("card is not valid JSON: %v\n%s", err, raw)
	}
	return payload
}

// container digs out the single container block.
func container(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	blocks, ok := payload["blocks"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("expected exactly one top-level block, got %v", payload["blocks"])
	}
	block, ok := blocks[0].(map[string]any)
	if !ok {
		t.Fatalf("block is not an object: %v", blocks[0])
	}
	return block
}

const sampleDiff = " chat:\n-  enabled: false\n+  enabled: true\n"

// TestPendingCardShape pins the layout against the shape that was specified:
// a non-collapsible wide container with a header divider, a diff-highlighted
// preformatted block, and the two buttons.
func TestPendingCardShape(t *testing.T) {
	raw, err := renderer(t).Pending("corr-1", sampleDiff)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	block := container(t, decodeBlocks(t, raw))

	if got := block["type"]; got != "container" {
		t.Errorf("block type = %v, want container", got)
	}
	if got := block["block_id"]; got != ContainerBlockID {
		t.Errorf("block_id = %v, want %q", got, ContainerBlockID)
	}
	// The card is a decision, so it must not arrive folded shut — an admin
	// should not have to expand a message to discover they are being asked
	// something.
	if got := block["is_collapsible"]; got != false {
		t.Errorf("is_collapsible = %v, want false", got)
	}
	if got := block["default_collapsed"]; got != false {
		t.Errorf("default_collapsed = %v, want false", got)
	}
	if got := block["has_header_divider"]; got != true {
		t.Errorf("has_header_divider = %v, want true", got)
	}
	if got := block["width"]; got != "wide" {
		t.Errorf("width = %v, want wide", got)
	}

	body := string(raw)
	if !strings.Contains(body, `"language": "diff"`) {
		t.Errorf("the diff is not syntax-highlighted as a diff:\n%s", body)
	}
	if !strings.Contains(body, "Apply Modifications") || !strings.Contains(body, "Rollback") {
		t.Errorf("card is missing its buttons:\n%s", body)
	}
	if !strings.Contains(body, ActionID("corr-1", ActionApply)) {
		t.Errorf("apply button carries no correlated action_id:\n%s", body)
	}
	if !strings.Contains(body, ActionID("corr-1", ActionRollback)) {
		t.Errorf("rollback button carries no correlated action_id:\n%s", body)
	}
}

// TestPendingCardStatesTheImpact is a correctness requirement, not a cosmetic
// one: a soft reload tears down every agent, and an admin who approves without
// being told that was misled by the card.
func TestPendingCardStatesTheImpact(t *testing.T) {
	raw, err := renderer(t).Pending("corr-1", sampleDiff)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if !strings.Contains(string(raw), "will be stopped") {
		t.Errorf("card does not warn that in-flight work will be stopped:\n%s", raw)
	}
}

// TestDiffSurvivesRendering checks the diff reaches Slack intact. It is
// newline- and prefix-heavy, and a template that escaped it wrongly would
// produce a card that renders as one unreadable line.
func TestDiffSurvivesRendering(t *testing.T) {
	diff := " a:\n-  b: 1\n+  b: 2\n \"quoted\": true\n\tliteral-tab\n"
	raw, err := renderer(t).Pending("corr-1", diff)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}

	// Walk to the preformatted element and compare the decoded text, which is
	// the only check that proves escaping round-trips rather than merely
	// producing valid JSON.
	block := container(t, decodeBlocks(t, raw))
	children, ok := block["child_blocks"].([]any)
	if !ok || len(children) == 0 {
		t.Fatalf("no child blocks: %v", block["child_blocks"])
	}
	rich, ok := children[0].(map[string]any)
	if !ok {
		t.Fatalf("first child is not an object: %v", children[0])
	}
	elements, ok := rich["elements"].([]any)
	if !ok || len(elements) < 2 {
		t.Fatalf("expected an intro and a preformatted element: %v", rich["elements"])
	}
	pre, ok := elements[1].(map[string]any)
	if !ok {
		t.Fatalf("second element is not an object: %v", elements[1])
	}
	preElements, ok := pre["elements"].([]any)
	if !ok || len(preElements) != 1 {
		t.Fatalf("preformatted block malformed: %v", pre["elements"])
	}
	text, ok := preElements[0].(map[string]any)["text"].(string)
	if !ok {
		t.Fatalf("preformatted text missing: %v", preElements[0])
	}
	if text != diff {
		t.Errorf("diff did not survive rendering:\n got %q\nwant %q", text, diff)
	}
}

// TestSettledCardDropsTheButtons stops a decided card from being clicked again
// — the second click would find no pending decision and read as a bug.
func TestSettledCardDropsTheButtons(t *testing.T) {
	raw, err := renderer(t).Settled(sampleDiff, "Approved by <@U1>; reloading the configuration.")
	if err != nil {
		t.Fatalf("Settled: %v", err)
	}
	body := string(raw)
	if strings.Contains(body, "Apply Modifications") || strings.Contains(body, ActionPrefix) {
		t.Errorf("a settled card still carries live buttons:\n%s", body)
	}
	if !strings.Contains(body, "reloading the configuration") {
		t.Errorf("settled card does not record the outcome:\n%s", body)
	}
	// The diff stays: the DM is the audit record of what was decided.
	if !strings.Contains(body, "enabled: true") {
		t.Errorf("settled card dropped the diff it was deciding on:\n%s", body)
	}
}

// TestActionIDRoundTrip pins the routing contract. A mis-parsed action_id sends
// a click to the wrong decision, or to none.
func TestActionIDRoundTrip(t *testing.T) {
	for _, action := range []Action{ActionApply, ActionRollback} {
		id := ActionID("abc-123", action)
		corr, got, ok := ParseActionID(id)
		if !ok {
			t.Fatalf("ParseActionID(%q) did not parse", id)
		}
		if corr != "abc-123" || got != action {
			t.Errorf("ParseActionID(%q) = (%q, %q), want (abc-123, %q)", id, corr, got, action)
		}
	}

	for _, id := range []string{
		"", "something_else", ActionPrefix, ActionPrefix + "apply_", ActionPrefix + "unknown_abc",
	} {
		if _, _, ok := ParseActionID(id); ok {
			t.Errorf("ParseActionID(%q) accepted an id it should not have", id)
		}
	}
}

// TestPlainTextFallbackCarriesEverything covers the degraded path taken when no
// raw-blocks client is wired: it must still show the diff and the warning,
// because an admin on that path is making the same decision.
func TestPlainTextFallbackCarriesEverything(t *testing.T) {
	text := PlainText(sampleDiff)
	if !strings.Contains(text, "enabled: true") {
		t.Errorf("fallback dropped the diff:\n%s", text)
	}
	if !strings.Contains(text, "will be stopped") {
		t.Errorf("fallback dropped the impact warning:\n%s", text)
	}
	if !strings.Contains(text, "```") {
		t.Errorf("fallback does not fence the diff, so it will reflow:\n%s", text)
	}
}
