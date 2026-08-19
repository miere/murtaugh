package askcard

import (
	"encoding/json"
	"strings"
	"testing"

	slackgo "github.com/slack-go/slack"

	"github.com/miere/murtaugh/assets"
)

func testRenderer() *Renderer { return NewRenderer("", assets.FS) }

// twoQuestions is the canvas example: one single-select, one multi-select.
func twoQuestions() Spec {
	return Spec{
		Questions: []Question{
			{
				Key:      "q0",
				Header:   "Storage Engine",
				Question: "Which database engine should we use?",
				Options: []Option{
					{Label: "PostgreSQL", Description: "Our existing transactional database."},
					{Label: "Redis", Description: "A Redis stream for fast ephemeral delivery."},
				},
			},
			{
				Key:         "q1",
				Header:      "Data Retention",
				Question:    "How should we handle historical data?",
				MultiSelect: true,
				Options: []Option{
					{Label: "Auto-archive", Description: "Move to cold storage after 30 days."},
					{Label: "Hard delete", Description: "TTL deletion after 14 days."},
					{Label: "User-dismissed", Description: "Delete on mark-as-read."},
				},
			},
		},
	}
}

// decode renders a template and unmarshals it, failing the test on invalid JSON.
// Comma placement across the nested {{ range }}/{{ if }} blocks is the fragile
// part of a hand-written JSON template, so every render is parsed rather than
// string-matched.
func decode(t *testing.T, ref string, data cardData) map[string]any {
	t.Helper()
	raw, err := testRenderer().render(ref, data)
	if err != nil {
		t.Fatalf("render %s: %v", ref, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("render %s produced invalid JSON: %v\n---\n%s", ref, err, raw)
	}
	return doc
}

// childBlocks digs out the container's children, which is where every card puts
// its actual content.
func childBlocks(t *testing.T, doc map[string]any) []any {
	t.Helper()
	blocks, ok := doc["blocks"].([]any)
	if !ok || len(blocks) == 0 {
		t.Fatalf("no blocks in %v", doc)
	}
	container, ok := blocks[0].(map[string]any)
	if !ok {
		t.Fatalf("first block is not an object: %v", blocks[0])
	}
	if got := container["type"]; got != "container" {
		t.Fatalf("first block type = %v, want container", got)
	}
	children, _ := container["child_blocks"].([]any)
	return children
}

func blocksOfType(children []any, want string) []map[string]any {
	var out []map[string]any
	for _, c := range children {
		b, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if b["type"] == want {
			out = append(out, b)
		}
	}
	return out
}

func TestPendingCardRendersValidJSON(t *testing.T) {
	f := &Flow{}
	doc := decode(t, PendingTemplate, f.data(twoQuestions(), "corr", StatePending, "", "", nil))
	children := childBlocks(t, doc)

	inputs := blocksOfType(children, "input")
	if len(inputs) != 2 {
		t.Fatalf("got %d input blocks, want 2", len(inputs))
	}
	if len(blocksOfType(children, "callout")) != 0 {
		t.Error("a card with no validation error must not render a callout")
	}

	// The single-select renders radio buttons, the multi-select checkboxes.
	first := inputs[0]["element"].(map[string]any)
	if first["type"] != "radio_buttons" {
		t.Errorf("q0 element = %v, want radio_buttons", first["type"])
	}
	second := inputs[1]["element"].(map[string]any)
	if second["type"] != "checkboxes" {
		t.Errorf("q1 element = %v, want checkboxes", second["type"])
	}
	if opts, _ := second["options"].([]any); len(opts) != 3 {
		t.Errorf("q1 options = %d, want 3", len(opts))
	}
	// Nothing pre-selected on a first render.
	if _, ok := first["initial_option"]; ok {
		t.Error("a first render must not pre-select an option")
	}

	// The block_id is how a submission is mapped back to its question.
	if got := inputs[0]["block_id"]; got != inputPrefix+"q0" {
		t.Errorf("block_id = %v, want %s", got, inputPrefix+"q0")
	}

	actions := blocksOfType(children, "actions")
	if len(actions) != 1 {
		t.Fatalf("got %d actions blocks, want 1", len(actions))
	}
	elems, _ := actions[0]["elements"].([]any)
	if len(elems) != 2 {
		t.Fatalf("got %d buttons, want Submit and Chat", len(elems))
	}
	submit := elems[0].(map[string]any)
	if got := submit["action_id"]; got != ActionID("corr", ActionSubmit) {
		t.Errorf("submit action_id = %v", got)
	}
}

// The validation re-render must carry existing answers back into the inputs.
// Without initial_option/initial_options a chat.update resets every control, so
// a user who answered 1 of 2 questions would watch that answer vanish the moment
// the callout appeared.
func TestPendingCardWithValidationErrorPreservesAnswers(t *testing.T) {
	f := &Flow{}
	selected := map[string][]string{
		"q1": {"Hard delete", "User-dismissed"},
	}
	doc := decode(t, PendingTemplate,
		f.data(twoQuestions(), "corr", StatePending, "Question 1 still needs an answer.", "", selected))
	children := childBlocks(t, doc)

	callouts := blocksOfType(children, "callout")
	if len(callouts) != 1 {
		t.Fatalf("got %d callouts, want 1", len(callouts))
	}
	if got := callouts[0]["background_color"]; got != "pink" {
		t.Errorf("callout background = %v, want pink", got)
	}

	inputs := blocksOfType(children, "input")
	// q0 was left unanswered, so it must stay unselected.
	if _, ok := inputs[0]["element"].(map[string]any)["initial_option"]; ok {
		t.Error("q0 was unanswered but rendered an initial_option")
	}
	// q1's two ticks must come back.
	initial, ok := inputs[1]["element"].(map[string]any)["initial_options"].([]any)
	if !ok {
		t.Fatal("q1 lost its answers: no initial_options")
	}
	if len(initial) != 2 {
		t.Fatalf("q1 initial_options = %d, want 2", len(initial))
	}
	got := map[string]bool{}
	for _, o := range initial {
		got[o.(map[string]any)["value"].(string)] = true
	}
	if !got["Hard delete"] || !got["User-dismissed"] {
		t.Errorf("initial_options values = %v", got)
	}
}

// A single-select answer round-trips through initial_option (singular), which is
// a different field from the multi-select case and so a separate failure mode.
func TestPendingCardPreservesSingleSelectAnswer(t *testing.T) {
	f := &Flow{}
	doc := decode(t, PendingTemplate, f.data(twoQuestions(), "corr", StatePending,
		"Question 2 still needs an answer.", "", map[string][]string{"q0": {"Redis"}}))
	inputs := blocksOfType(childBlocks(t, doc), "input")

	initial, ok := inputs[0]["element"].(map[string]any)["initial_option"].(map[string]any)
	if !ok {
		t.Fatal("q0 lost its answer: no initial_option")
	}
	if got := initial["value"]; got != "Redis" {
		t.Errorf("initial_option value = %v, want Redis", got)
	}
}

func TestAnsweredCardRendersValidJSON(t *testing.T) {
	f := &Flow{}
	answers := map[string][]string{
		"q0": {"PostgreSQL"},
		"q1": {"Hard delete", "User-dismissed"},
	}
	doc := decode(t, AnsweredTemplate, f.data(twoQuestions(), "corr", StateAnswered, "", "U123", answers))
	children := childBlocks(t, doc)

	sections := blocksOfType(children, "section")
	if len(sections) != 2 {
		t.Fatalf("got %d sections, want one per question", len(sections))
	}
	text := sections[1]["text"].(map[string]any)["text"].(string)
	for _, want := range []string{"Hard delete", "User-dismissed", "✓"} {
		if !strings.Contains(text, want) {
			t.Errorf("answered section missing %q; got %q", want, text)
		}
	}
	// A terminal card must not carry inputs or buttons.
	if len(blocksOfType(children, "input")) != 0 || len(blocksOfType(children, "actions")) != 0 {
		t.Error("the answered card still has live controls")
	}
	ctxs := blocksOfType(children, "context")
	if len(ctxs) != 1 {
		t.Fatalf("got %d context blocks, want the answered-by line", len(ctxs))
	}
}

func TestChatCardRendersValidJSON(t *testing.T) {
	f := &Flow{}
	doc := decode(t, ChatTemplate, f.data(twoQuestions(), "corr", StateChat, "", "U123", nil))
	children := childBlocks(t, doc)

	sections := blocksOfType(children, "section")
	if len(sections) != 1 {
		t.Fatalf("got %d sections, want 1", len(sections))
	}
	text := sections[0]["text"].(map[string]any)["text"].(string)
	if !strings.Contains(text, "Which database engine") {
		t.Errorf("chat card should restate the questions; got %q", text)
	}
	if len(blocksOfType(children, "actions")) != 0 {
		t.Error("the chat card still has buttons")
	}
}

// Every question, option label and description is agent-supplied, so a value
// that closes its JSON string literal must not be able to inject block
// structure. text/template does no escaping — the json/jsonstr funcs are the
// only thing standing between an option label and an attacker-shaped card.
func TestAgentSuppliedTextCannotInjectBlocks(t *testing.T) {
	nasty := `oops", "type": "actions", "elements": [{"x":"`
	spec := Spec{
		Title: nasty,
		Questions: []Question{{
			Key:      "q0",
			Header:   nasty,
			Question: nasty,
			Options: []Option{
				{Label: nasty, Description: nasty},
				{Label: "fine", Description: ""},
			},
		}},
	}
	f := &Flow{}
	doc := decode(t, PendingTemplate, f.data(spec, "corr", StatePending, nasty, "", map[string][]string{"q0": {nasty}}))
	children := childBlocks(t, doc)

	// One callout, one input, one actions block — and nothing smuggled in.
	if got := len(blocksOfType(children, "actions")); got != 1 {
		t.Fatalf("got %d actions blocks, want exactly 1 (injection created extras)", got)
	}
	inputs := blocksOfType(children, "input")
	if len(inputs) != 1 {
		t.Fatalf("got %d inputs, want 1", len(inputs))
	}
	// The label survives intact rather than being truncated at the quote.
	opts := inputs[0]["element"].(map[string]any)["options"].([]any)
	if got := opts[0].(map[string]any)["value"]; got != nasty {
		t.Errorf("option value = %q, want the literal text back", got)
	}
}

func TestActionIDRoundTrip(t *testing.T) {
	for _, action := range []Action{ActionSubmit, ActionChat} {
		id := ActionID("abc123", action)
		corr, got, ok := ParseActionID(id)
		if !ok || corr != "abc123" || got != action {
			t.Errorf("ParseActionID(%q) = (%q, %q, %v)", id, corr, got, ok)
		}
	}
	if _, _, ok := ParseActionID("murtaugh_auth:abc:deny"); ok {
		t.Error("an auth action_id must not parse as an ask action")
	}
	if _, _, ok := ParseActionID("not-ours"); ok {
		t.Error("a foreign action_id must not parse")
	}
}

func TestParseSubmissionReadsBothWidgets(t *testing.T) {
	ic := slackgo.InteractionCallback{
		Type: slackgo.InteractionTypeBlockActions,
		BlockActionState: &slackgo.BlockActionStates{
			Values: map[string]map[string]slackgo.BlockAction{
				inputPrefix + "q0": {
					inputPrefix + "q0": {
						SelectedOption: slackgo.OptionBlockObject{Value: "PostgreSQL"},
					},
				},
				inputPrefix + "q1": {
					inputPrefix + "q1": {
						SelectedOptions: []slackgo.OptionBlockObject{
							{Value: "Hard delete"},
							{Value: "User-dismissed"},
						},
					},
				},
				// A block from some other card in the same message must be ignored.
				"someone_elses_block": {
					"whatever": {SelectedOption: slackgo.OptionBlockObject{Value: "nope"}},
				},
			},
		},
	}
	got := ParseSubmission(ic)
	if len(got) != 2 {
		t.Fatalf("got %d answers, want 2: %v", len(got), got)
	}
	if len(got["q0"]) != 1 || got["q0"][0] != "PostgreSQL" {
		t.Errorf("q0 = %v", got["q0"])
	}
	if len(got["q1"]) != 2 {
		t.Errorf("q1 = %v", got["q1"])
	}
}

func TestParseSubmissionWithNoState(t *testing.T) {
	if got := ParseSubmission(slackgo.InteractionCallback{}); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestUnansweredFindsGaps(t *testing.T) {
	spec := twoQuestions()
	missing := unanswered(spec.Questions, map[string][]string{"q1": {"Hard delete"}})
	if len(missing) != 1 || missing[0].Key != "q0" {
		t.Fatalf("missing = %+v, want just q0", missing)
	}
	if got := len(unanswered(spec.Questions, map[string][]string{"q0": {"Redis"}, "q1": {"Hard delete"}})); got != 0 {
		t.Errorf("a fully answered form reported %d gaps", got)
	}
	// An empty slice counts as unanswered, not as an answer.
	if got := len(unanswered(spec.Questions, map[string][]string{"q0": {}, "q1": {}})); got != 2 {
		t.Errorf("empty selections reported %d gaps, want 2", got)
	}
}

func TestValidationMessageNamesTheGaps(t *testing.T) {
	spec := twoQuestions()
	one := validationMessage(unanswered(spec.Questions, map[string][]string{"q1": {"x"}}))
	if !strings.Contains(one, "Question 1 ") {
		t.Errorf("single-gap message = %q, want it to name question 1", one)
	}
	both := validationMessage(unanswered(spec.Questions, nil))
	if !strings.Contains(both, "1 and 2") {
		t.Errorf("two-gap message = %q, want it to name both", both)
	}
	if !strings.Contains(one, "Chat About This") {
		t.Errorf("the message should point at the escape hatch; got %q", one)
	}
}
