package remote

import (
	"context"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
)

func promptWithSignIn(t *testing.T, owner string) ([]agent.Event, string) {
	t.Helper()
	client, node, logs := dial(t, nodeConfig{}, Options{Owner: owner})
	events, err := client.Prompt(context.Background(), "session-42", agent.PromptRequest{Text: "sign in", Channel: "C1", Thread: "1.1", User: "U9"})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	stream := receive(t, "the accepted stream", node.streams)
	node.emit(stream, agentwire.Event{Type: agentwire.EventSignIn, SignIn: &agentwire.SignInRequest{ID: "sign-in-1", Tool: "gcp-mcp", Profile: "gcloud", URL: "https://accounts.example.com"}})
	node.emit(stream, agentwire.Event{Type: agentwire.EventText, Text: "after"})
	node.end(stream)
	var got []agent.Event
	for ev := range events {
		got = append(got, ev)
	}
	return got, logs.String()
}

// A node's sign-in is only ever drawn for the owner its credential names.
func TestASignInIsStampedWithTheConnectionsOwner(t *testing.T) {
	events, _ := promptWithSignIn(t, "UOWNER")
	if len(events) == 0 || events[0].SignIn == nil || events[0].SignIn.Owner != "UOWNER" {
		t.Fatalf("the sign-in arrived as %+v", events)
	}
}

// A connection that names no owner has nobody to ask, and must not fall back to
// the gateway admin, who does not own the node's credentials.
func TestASignInFromANodeWithNoOwnerIsRefused(t *testing.T) {
	events, logs := promptWithSignIn(t, "")
	for _, ev := range events {
		if ev.SignIn != nil {
			t.Fatalf("a sign-in from a node with no owner was handed on to be drawn: %+v", ev.SignIn)
		}
	}
	if len(events) != 1 || events[0].Text != "after" {
		t.Fatalf("the turn went on as %+v", events)
	}
	if !strings.Contains(logs, "names no owner") {
		t.Fatalf("the refusal was not logged:\n%s", logs)
	}
}
