package claudecode

import (
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
)

func TestComposePrompt_FoldsHistory(t *testing.T) {
	got := composePrompt(agent.PromptRequest{
		Text:    "is this true?",
		History: "<canvas-context>You were mentioned inside a Slack canvas.</canvas-context>\n\n<thread-transcript>…</thread-transcript>",
	})
	if !strings.Contains(got, "Slack canvas") {
		t.Fatalf("history (canvas context) must be folded into the prompt, got:\n%s", got)
	}
	if !strings.Contains(got, "is this true?") {
		t.Fatalf("the user's text must be present, got:\n%s", got)
	}
	// History comes first so it frames the question.
	if strings.Index(got, "Slack canvas") > strings.Index(got, "is this true?") {
		t.Fatalf("history should precede the user text, got:\n%s", got)
	}
}

func TestComposePrompt_NoHistoryIsJustText(t *testing.T) {
	if got := composePrompt(agent.PromptRequest{Text: "hi"}); got != "hi" {
		t.Fatalf("with no history, compose should be the text alone, got %q", got)
	}
	if got := composePrompt(agent.PromptRequest{Text: "hi", History: "   "}); got != "hi" {
		t.Fatalf("blank history should be ignored, got %q", got)
	}
}
