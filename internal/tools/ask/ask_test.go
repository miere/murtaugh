package ask

import (
	"context"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
)

type fakeDisplay struct {
	answer agent.DisplayAnswer
	err    error
	loc    agent.TurnLocation
	req    agent.QuestionRequest
	calls  int
}

func (d *fakeDisplay) Question(_ context.Context, loc agent.TurnLocation, req agent.QuestionRequest) (agent.DisplayAnswer, error) {
	d.calls++
	d.loc, d.req = loc, req
	return d.answer, d.err
}

func locatedCtx() context.Context {
	return agent.WithTurnLocation(context.Background(), agent.TurnLocation{ChannelID: "C1", ThreadTS: "t1"})
}

func invoke(t *testing.T, d *fakeDisplay, args map[string]any) Result {
	t.Helper()
	out, err := New(d).Invoke(locatedCtx(), args)
	if err != nil {
		t.Fatalf("Invoke error: %v", err)
	}
	return out.(Result)
}

func TestInvoke_NilDisplayErrors(t *testing.T) {
	_, err := New(nil).Invoke(locatedCtx(), map[string]any{"question": "q", "options": []any{"a", "b"}})
	if err == nil {
		t.Fatal("expected an error when nothing can draw the question")
	}
}

// Refusing up front, before anything is drawn, is what keeps a headless run from
// raising a question nobody can answer.
func TestInvoke_RequiresSlackLocation(t *testing.T) {
	d := &fakeDisplay{}
	_, err := New(d).Invoke(context.Background(), map[string]any{"question": "q", "options": []any{"a", "b"}})
	if err == nil || err.Error() != "Error: the ask tool only works inside a Slack conversation" {
		t.Fatalf("expected the Slack-conversation refusal, got %v", err)
	}
	if d.calls != 0 {
		t.Fatal("a question with no conversation reached the display")
	}
}

func TestInvoke_RequiresTwoOptions(t *testing.T) {
	_, err := New(&fakeDisplay{}).Invoke(locatedCtx(), map[string]any{"question": "q", "options": []any{"only one"}})
	if err == nil {
		t.Fatal("expected an error for fewer than two options")
	}
}

func TestInvoke_PlainQuestionReturnsTheChoice(t *testing.T) {
	d := &fakeDisplay{answer: agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Answers: map[string][]string{"q0": {"Approve"}}}}
	got := invoke(t, d, map[string]any{"question": "Ship it?", "options": []any{"Approve", "Deny"}})

	if d.loc.ChannelID != "C1" || d.loc.ThreadTS != "t1" {
		t.Fatalf("the display was handed %+v, want the turn's own location", d.loc)
	}
	if !d.req.Plain() || d.req.Questions[0].Question != "Ship it?" || len(d.req.Questions[0].Options) != 2 {
		t.Fatalf("the display was asked %+v", d.req)
	}
	if !got.Answered || got.Choice != "Approve" || len(got.Answers) != 0 {
		t.Fatalf("unexpected result: %+v", got)
	}
}

// A single plain question sent in Claude's shape is still a plain question, so it
// keeps the cheaper button prompt and the single-choice result.
func TestInvoke_SinglePlainQuestionStaysPlain(t *testing.T) {
	d := &fakeDisplay{answer: agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Answers: map[string][]string{"q0": {"Yes"}}}}
	got := invoke(t, d, map[string]any{
		"questions": []any{map[string]any{"label": "Ship?", "options": []any{"Yes", "No"}}},
	})
	if !d.req.Plain() {
		t.Fatalf("a lone plain question was not sent as plain: %+v", d.req)
	}
	if !got.Answered || got.Choice != "Yes" {
		t.Fatalf("expected a single-choice result, got %+v", got)
	}
}

func TestInvoke_MultiQuestionReturnsEachAnswer(t *testing.T) {
	d := &fakeDisplay{answer: agent.DisplayAnswer{
		Outcome: agent.DisplayAnswered,
		Answers: map[string][]string{"q0": {"Production"}, "q1": {"US", "EU"}},
		UserID:  "U1",
	}}
	got := invoke(t, d, map[string]any{
		"title": "Deploy",
		"questions": []any{
			map[string]any{"label": "Env?", "options": []any{"Staging", "Production"}},
			map[string]any{"label": "Regions?", "multiSelect": true, "options": []any{"US", "EU"}},
		},
	})
	if d.req.Plain() || d.req.Title != "Deploy" || len(d.req.Questions) != 2 {
		t.Fatalf("the display was asked %+v", d.req)
	}
	if !got.Answered || len(got.Answers) != 2 {
		t.Fatalf("unexpected result: %+v", got)
	}
	if got.Answers[0].Question != "Env?" || len(got.Answers[0].Choices) != 1 || got.Answers[0].Choices[0] != "Production" {
		t.Fatalf("env answer wrong: %+v", got.Answers[0])
	}
	if len(got.Answers[1].Choices) != 2 {
		t.Fatalf("regions answer wrong: %+v", got.Answers[1])
	}
	if got.UserID != "U1" {
		t.Errorf("UserID = %q, want U1 — the model should be able to attribute the decision", got.UserID)
	}
}

func TestInvoke_UnansweredOutcomesAreNotAnswers(t *testing.T) {
	for _, outcome := range []agent.DisplayOutcome{agent.DisplayTimedOut, agent.DisplayDismissed} {
		got := invoke(t, &fakeDisplay{answer: agent.DisplayAnswer{Outcome: outcome}},
			map[string]any{"question": "q", "options": []any{"a", "b"}})
		if got.Answered || got.Note == "" {
			t.Errorf("%s came back as %+v; it must be unanswered and say why", outcome, got)
		}
	}
}

// A gateway that finds no conversation behind the turn answers no_conversation,
// and the model must read the same refusal it would have got up front.
func TestInvoke_GatewayRefusalReadsAsTheHeadlessRefusal(t *testing.T) {
	_, err := New(&fakeDisplay{answer: agent.DisplayAnswer{Outcome: agent.DisplayNoConversation}}).
		Invoke(locatedCtx(), map[string]any{"question": "q", "options": []any{"a", "b"}})
	if err == nil || err.Error() != "Error: the ask tool only works inside a Slack conversation" {
		t.Fatalf("expected the Slack-conversation refusal, got %v", err)
	}
}

// "Chat About This" is not a refusal: the user wants to talk the options over,
// so the model must read it as their question back.
func TestInvoke_ChatAboutThisAsksTheModelToDiscuss(t *testing.T) {
	got := invoke(t, &fakeDisplay{answer: agent.DisplayAnswer{Outcome: agent.DisplayChat}}, map[string]any{
		"questions": []any{
			map[string]any{"label": "Env?", "options": []any{"Staging", "Production"}},
			map[string]any{"label": "Regions?", "multiSelect": true, "options": []any{"US", "EU"}},
		},
	})
	if got.Answered {
		t.Fatalf("a chat request must not report as answered: %+v", got)
	}
	note := got.String()
	if !strings.Contains(note, "Can we chat about this?") {
		t.Errorf("note should carry the user's question verbatim; got %q", note)
	}
	for _, want := range []string{"Env?", "Regions?"} {
		if !strings.Contains(note, want) {
			t.Errorf("note should restate %q; got %q", want, note)
		}
	}
	for _, banned := range []string{"did not respond", "dismissed", "denied"} {
		if strings.Contains(note, banned) {
			t.Errorf("note reads as a refusal (%q); got %q", banned, note)
		}
	}
}

// --- the Claude-facing surface ---------------------------------------------

// The advertised schema IS Claude's AskUserQuestion payload. This test is the
// contract: if it drifts, a Claude Code agent reaching for the tool it already
// knows starts getting argument errors, and the substitution stops being
// invisible. Every assertion below mirrors a field of Claude's own schema.
func TestInputSchemaMatchesClaudesPayload(t *testing.T) {
	schema := New(nil).InputSchema()

	if schema.Type != "object" {
		t.Fatalf("root type = %q, want object", schema.Type)
	}
	if len(schema.Required) != 1 || schema.Required[0] != "questions" {
		t.Fatalf("root required = %v, want [questions]", schema.Required)
	}
	// The legacy single-question shape must not be advertised: Invoke still
	// accepts it, but the model-facing contract is Claude's alone.
	for _, gone := range []string{"question", "options", "title"} {
		if _, ok := schema.Properties[gone]; ok {
			t.Errorf("schema advertises %q; the published shape must be Claude's only", gone)
		}
	}

	questions, ok := schema.Properties["questions"]
	if !ok {
		t.Fatal("no questions property")
	}
	if questions.MinItems == nil || *questions.MinItems != 1 {
		t.Error("questions should require at least 1")
	}
	if questions.MaxItems == nil || *questions.MaxItems != 4 {
		t.Error("questions should cap at 4, as Claude's does")
	}

	q := questions.Items
	wantRequired := map[string]bool{"header": true, "question": true, "options": true}
	if len(q.Required) != len(wantRequired) {
		t.Errorf("question required = %v, want header/question/options", q.Required)
	}
	for _, name := range q.Required {
		if !wantRequired[name] {
			t.Errorf("unexpected required field %q", name)
		}
	}
	if h := q.Properties["header"]; h == nil || h.MaxLength == nil || *h.MaxLength != 12 {
		t.Error("header should cap at 12 characters, as Claude's does")
	}
	if _, ok := q.Properties["multiSelect"]; !ok {
		t.Error("no multiSelect property")
	}
	if _, ok := q.Properties["freeText"]; ok {
		t.Error("freeText is not part of Claude's payload and must not be advertised")
	}

	opts := q.Properties["options"]
	if opts == nil {
		t.Fatal("no options property")
	}
	if opts.MinItems == nil || *opts.MinItems != 2 {
		t.Error("options should require at least 2")
	}
	if opts.MaxItems == nil || *opts.MaxItems != 4 {
		t.Error("options should cap at 4, as Claude's does")
	}
	// Options are objects, not bare strings — this is the shape change that
	// carries the per-option description onto the card.
	if opts.Items.Type != "object" {
		t.Fatalf("option type = %q, want object", opts.Items.Type)
	}
	for _, want := range []string{"label", "description"} {
		if _, ok := opts.Items.Properties[want]; !ok {
			t.Errorf("option has no %q property", want)
		}
	}
}

// Claude sends options as objects with a description. The description has to
// survive into the card, since it is doing the explanatory work the label cannot.
func TestParseOptionsAcceptsClaudeObjects(t *testing.T) {
	got := parseOptions([]any{
		map[string]any{"label": "PostgreSQL", "description": "Our existing transactional database."},
		map[string]any{"label": "Redis"},
		map[string]any{"description": "no label, unpickable"},
	})
	if len(got) != 2 {
		t.Fatalf("got %d options, want the 2 with labels: %+v", len(got), got)
	}
	if got[0].Label != "PostgreSQL" || got[0].Description != "Our existing transactional database." {
		t.Errorf("option 0 = %+v", got[0])
	}
	if got[1].Description != "" {
		t.Errorf("option 1 should have no description; got %q", got[1].Description)
	}
}

// The older bare-string shape still parses, so prompts written before the
// switch keep working even though the schema no longer advertises it.
func TestParseOptionsStillAcceptsBareStrings(t *testing.T) {
	got := parseOptions([]any{"Yes", "  ", "No"})
	if len(got) != 2 || got[0].Label != "Yes" || got[1].Label != "No" {
		t.Fatalf("got %+v, want Yes/No with the blank dropped", got)
	}
}

// `question` is Claude's field name and `label` the older one; both must reach
// the card, or the advertised schema and the parser disagree.
func TestParseQuestionsAcceptsBothFieldNames(t *testing.T) {
	got := parseQuestions([]any{
		map[string]any{
			"header":      "Storage",
			"question":    "Which engine?",
			"multiSelect": true,
			"options":     []any{map[string]any{"label": "PostgreSQL"}, map[string]any{"label": "Redis"}},
		},
		map[string]any{"label": "Legacy phrasing?", "options": []any{"a", "b"}},
		map[string]any{"options": []any{"a", "b"}}, // no text at all: dropped
	})
	if len(got) != 2 {
		t.Fatalf("got %d questions, want 2: %+v", len(got), got)
	}
	if got[0].Header != "Storage" || got[0].Question != "Which engine?" || !got[0].MultiSelect {
		t.Errorf("question 0 = %+v", got[0])
	}
	if got[1].Question != "Legacy phrasing?" {
		t.Errorf("question 1 = %+v", got[1])
	}
	// Keys are positional, which is what makes the card's validation message
	// name the right question numbers.
	if got[0].Key != "q0" || got[1].Key != "q1" {
		t.Errorf("keys = %q/%q, want q0/q1", got[0].Key, got[1].Key)
	}
}

// A header, or an option description, means the button path cannot render the
// question faithfully — both must route to the card.
func TestClaudeShapedQuestionsAreNotPlain(t *testing.T) {
	withHeader := parseQuestions([]any{
		map[string]any{"header": "Env", "question": "Which?", "options": []any{"a", "b"}},
	})
	if (agent.QuestionRequest{Questions: withHeader}).Plain() {
		t.Error("a question with a header must use the card: a button has nowhere to show it")
	}
	withDescription := parseQuestions([]any{
		map[string]any{"question": "Which?", "options": []any{
			map[string]any{"label": "a", "description": "the long explanation"},
			map[string]any{"label": "b"},
		}},
	})
	if (agent.QuestionRequest{Questions: withDescription}).Plain() {
		t.Error("an option description must use the card: a button has nowhere to show it")
	}
	plain := parseQuestions([]any{
		map[string]any{"question": "Ship?", "options": []any{"Yes", "No"}},
	})
	if !(agent.QuestionRequest{Questions: plain}).Plain() {
		t.Error("a lone plain question should still ride the cheaper button path")
	}
}
