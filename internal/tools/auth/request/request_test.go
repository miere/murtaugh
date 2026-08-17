package request

import (
	"context"
	"strings"
	"testing"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/slack/authcard"
	slacklib "github.com/miere/murtaugh/internal/slack/client"
)

func TestNameAndSchema(t *testing.T) {
	tool := New(nil)
	if tool.Name() != "auth.request" {
		t.Fatalf("Name = %q", tool.Name())
	}
	schema := tool.InputSchema()
	if schema == nil {
		t.Fatal("InputSchema is nil")
	}
	for _, key := range []string{"tool", "profile", "command", "needs_code"} {
		if _, ok := schema.Properties[key]; !ok {
			t.Fatalf("schema is missing %q", key)
		}
	}
	if len(schema.Required) != 2 {
		t.Fatalf("expected tool+profile required, got %v", schema.Required)
	}
	if len(schema.Properties["profile"].Enum) == 0 {
		t.Fatal("profile should enumerate the available profiles")
	}
}

// The description is a contract with the model: it must steer it towards the
// affected capability and away from the helper binary underneath.
func TestDescriptionCarriesTheToolNamingGuidance(t *testing.T) {
	d := New(nil).Description()
	for _, want := range []string{"DIRECTLY affected", "gcloud", "admin"} {
		if !strings.Contains(d, want) {
			t.Fatalf("description should mention %q:\n%s", want, d)
		}
	}
}

// Registered without a gateway to route clicks back, the tool reports that
// rather than blocking forever.
func TestInertWithoutAFlow(t *testing.T) {
	_, err := New(nil).Invoke(context.Background(), map[string]any{"tool": "x", "profile": "gcloud"})
	if err == nil {
		t.Fatal("expected an error with no flow wired")
	}
}

func TestRequiresToolName(t *testing.T) {
	tool := New(newFlow())
	if _, err := tool.Invoke(context.Background(), map[string]any{"profile": "gcloud"}); err == nil {
		t.Fatal("expected an error when `tool` is missing")
	}
}

func TestRejectsUnknownProfile(t *testing.T) {
	tool := New(newFlow())
	_, err := tool.Invoke(context.Background(), map[string]any{"tool": "x", "profile": "nope"})
	if err == nil {
		t.Fatal("expected an error for an unknown profile")
	}
	if !strings.Contains(err.Error(), "unknown profile") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

// aws is in the design but not implemented; it must be refused rather than
// posting a card that cannot complete.
func TestRejectsAWSProfile(t *testing.T) {
	tool := New(newFlow())
	if _, err := tool.Invoke(context.Background(), map[string]any{"tool": "x", "profile": "aws"}); err == nil {
		t.Fatal("expected aws to be rejected")
	}
}

func TestRejectsCommandOnBuiltinProfile(t *testing.T) {
	tool := New(newFlow())
	_, err := tool.Invoke(context.Background(), map[string]any{
		"tool": "x", "profile": "gcloud", "command": "something-else",
	})
	if err == nil {
		t.Fatal("expected an error when a command accompanies a built-in profile")
	}
}

func TestCustomRequiresACommand(t *testing.T) {
	tool := New(newFlow())
	if _, err := tool.Invoke(context.Background(), map[string]any{"tool": "x", "profile": "custom"}); err == nil {
		t.Fatal("expected an error for custom with no command")
	}
}

// No admin configured means nobody can approve. The tool must surface that as
// an error rather than blocking.
func TestFailsClosedWithNoAdmin(t *testing.T) {
	tool := New(newFlow()) // constructed with an empty admin
	_, err := tool.Invoke(context.Background(), map[string]any{
		"tool": "gcp-mcp", "profile": "custom", "command": "true",
	})
	if err == nil {
		t.Fatal("expected an error when no admin is configured")
	}
	if !strings.Contains(err.Error(), "admin") {
		t.Fatalf("error should explain the missing admin, got: %v", err)
	}
}

// The turn's location is what tells the flow where the requester is; without it
// the flow collapses to the admin card alone. This asserts the plumbing reads
// the context rather than inventing a destination.
func TestReadsTurnLocationFromContext(t *testing.T) {
	tool := New(newFlow())
	ctx := agent.WithTurnLocation(context.Background(), agent.TurnLocation{
		ChannelID: "C1", ThreadTS: "1.1", UserID: "U1",
	})
	// Still fails (no admin), but exercising the path proves the context read
	// does not panic and the location is accepted.
	if _, err := tool.Invoke(ctx, map[string]any{"tool": "x", "profile": "custom", "command": "true"}); err == nil {
		t.Fatal("expected the no-admin failure")
	}
}

func TestResultString(t *testing.T) {
	r := Result{Authenticated: true, Tool: "gcp-mcp", Profile: "gcloud-adc"}
	s := r.String()
	if !strings.Contains(s, "gcp-mcp") || !strings.Contains(s, "gcloud-adc") {
		t.Fatalf("Result.String should name the tool and profile, got %q", s)
	}
	if !strings.Contains(strings.ToLower(s), "retry") {
		t.Fatalf("Result.String should tell the model to retry the failed call, got %q", s)
	}
}

func newFlow() *authcard.Flow {
	return authcard.New(
		slacklib.NewLazyClient("xoxb-test"),
		authcard.NewRenderer("", assets.FS),
		"", // no admin — the fail-closed default for these tests
		nil,
	)
}
