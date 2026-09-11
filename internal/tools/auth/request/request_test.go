package request

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
)

type fakeDisplay struct {
	refuse  bool
	answers []agent.DisplayAnswer
	prompts chan *agent.SignInPrompt

	mu      sync.Mutex
	raised  []agent.SignInRequest
	settled []agent.SignInState
	links   []string
}

func (d *fakeDisplay) SignIn(_ context.Context, req agent.SignInRequest) (*agent.SignInPrompt, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.raised = append(d.raised, req)
	if d.refuse {
		return nil, false
	}
	prompt := &agent.SignInPrompt{Request: req, Answer: make(chan agent.DisplayAnswer, len(d.answers)+1)}
	for _, a := range d.answers {
		prompt.Answer <- a
	}
	if d.prompts != nil {
		d.prompts <- prompt
	}
	return prompt, true
}

func (d *fakeDisplay) SettleSignIn(_ context.Context, update agent.SignInSettled) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.settled = append(d.settled, update.State)
	if update.URL != "" {
		d.links = append(d.links, update.URL)
	}
}

func (d *fakeDisplay) states() []agent.SignInState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]agent.SignInState(nil), d.settled...)
}

func inConversation() context.Context {
	return agent.WithTurnLocation(context.Background(), agent.TurnLocation{ChannelID: "C1", ThreadTS: "1.1", UserID: "U1"})
}

func codeFlow(tail string) map[string]any {
	return map[string]any{
		"tool":       "gcp-mcp",
		"profile":    "custom",
		"command":    `echo "Go to https://example.com/auth?x=1"; ` + tail,
		"needs_code": true,
	}
}

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

// The description is a contract with the model: it names the affected
// capability rather than the helper binary, and says who is asked.
func TestDescriptionCarriesTheToolNamingGuidance(t *testing.T) {
	d := New(nil).Description()
	for _, want := range []string{"DIRECTLY affected", "gcloud", "owner"} {
		if !strings.Contains(d, want) {
			t.Fatalf("description should mention %q:\n%s", want, d)
		}
	}
}

func TestInertWithoutADisplay(t *testing.T) {
	if _, err := New(nil).Invoke(inConversation(), map[string]any{"tool": "x", "profile": "gcloud"}); err == nil {
		t.Fatal("expected an error with nothing to draw the sign-in")
	}
}

func TestRejectsBadArguments(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"no tool":              {"profile": "gcloud"},
		"unknown profile":      {"tool": "x", "profile": "nope"},
		"aws":                  {"tool": "x", "profile": "aws"},
		"command on a builtin": {"tool": "x", "profile": "gcloud", "command": "rm -rf /"},
		"custom with nothing":  {"tool": "x", "profile": "custom"},
	} {
		t.Run(name, func(t *testing.T) {
			display := &fakeDisplay{}
			if _, err := New(display).Invoke(inConversation(), args); err == nil {
				t.Fatal("the tool accepted bad arguments")
			}
			if len(display.raised) != 0 {
				t.Fatal("bad arguments still raised a sign-in")
			}
		})
	}
}

// Outside a conversation nobody can be shown the card, so nothing is started.
func TestRefusedOutsideAConversation(t *testing.T) {
	display := &fakeDisplay{}
	_, err := New(display).Invoke(context.Background(), codeFlow("read code"))
	if err == nil || !strings.Contains(err.Error(), "only works inside a Slack conversation") {
		t.Fatalf("a sign-in with no conversation was answered with %v", err)
	}
	if len(display.raised) != 0 {
		t.Fatal("a sign-in was raised with no conversation to draw it in")
	}
}

func TestWithNoConversationTheOwnerIsAskedDirectly(t *testing.T) {
	marker, command := marked(t)
	turn := &fakeDisplay{}
	owner := &fakeDisplay{answers: approvedThen(agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Code: "GOOD"})}
	out, err := New(turn).WithoutConversation(owner).Invoke(context.Background(),
		map[string]any{"tool": "vendor-mcp", "profile": "custom", "command": command, "needs_code": true})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if r, ok := out.(Result); !ok || !r.Authenticated || !exists(marker) {
		t.Fatalf("result = %+v", out)
	}
	if len(turn.raised) != 0 || len(owner.raised) != 1 || owner.raised[0].Command != command || owner.raised[0].URL != "" {
		t.Fatalf("raised %+v on the turn and %+v with the owner; want only the owner asked, to approve the command", turn.raised, owner.raised)
	}
	if got := owner.states(); got[0] != agent.SignInReady || got[len(got)-1] != agent.SignInSuccess {
		t.Fatalf("settled %v with the owner, want ready first and success last", got)
	}

	inTurn := &fakeDisplay{refuse: true}
	unused := &fakeDisplay{}
	if _, err := New(inTurn).WithoutConversation(unused).Invoke(inConversation(), codeFlow("read code")); err == nil {
		t.Fatal("a sign-in the conversation could not show succeeded")
	}
	if len(unused.raised) != 0 {
		t.Fatal("a sign-in raised in a conversation went to the owner's DM instead")
	}
}

func approvedThen(answers ...agent.DisplayAnswer) []agent.DisplayAnswer {
	return append([]agent.DisplayAnswer{{Outcome: agent.DisplayApproved, UserID: "UOWNER"}}, answers...)
}

// A built-in profile runs a command of ours, so it starts at once and its link
// goes out with the request.
func TestABuiltInSignInRunsAtOnceAndSettles(t *testing.T) {
	bin := t.TempDir()
	fake := "#!/bin/sh\necho \"Go to https://accounts.google.com/o/oauth2/auth?x=1\"\nread code\n[ \"$code\" = GOOD ]\n"
	if err := os.WriteFile(filepath.Join(bin, "gcloud"), []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake gcloud: %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	display := &fakeDisplay{answers: []agent.DisplayAnswer{{Outcome: agent.DisplayAnswered, Code: "GOOD", UserID: "UOWNER"}}}
	out, err := New(display).Invoke(inConversation(), map[string]any{"tool": "gcp-mcp", "profile": "gcloud"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if r, ok := out.(Result); !ok || !r.Authenticated {
		t.Fatalf("result = %+v", out)
	}
	want := agent.SignInRequest{Tool: "gcp-mcp", Profile: "gcloud", URL: "https://accounts.google.com/o/oauth2/auth?x=1", NeedsCode: true}
	if len(display.raised) != 1 || display.raised[0] != want {
		t.Fatalf("raised %+v, want %+v", display.raised, want)
	}
	if got := display.states(); len(got) != 2 || got[0] != agent.SignInWorking || got[1] != agent.SignInSuccess {
		t.Fatalf("settled %v, want working then success", got)
	}
}

// The request carries no environment; the process still runs in the agent's.
func TestTheSignInRunsInTheAgentsEnvironment(t *testing.T) {
	t.Setenv("MURTAUGH_SIGNIN_TEST", "node")
	display := &fakeDisplay{answers: approvedThen()}
	ctx := agent.WithTurnEnv(inConversation(), []string{"MURTAUGH_SIGNIN_TEST=agent"})
	_, _ = New(display).Invoke(ctx, map[string]any{
		"tool": "gcp-mcp", "profile": "custom", "command": `echo "https://example.com/auth?v=$MURTAUGH_SIGNIN_TEST"`,
	})
	if len(display.links) != 1 || display.links[0] != "https://example.com/auth?v=agent" {
		t.Fatalf("linked %+v; the sign-in did not run in the agent's environment", display.links)
	}
}

func TestAWrongCodeFailsClosed(t *testing.T) {
	display := &fakeDisplay{answers: approvedThen(agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Code: "BAD"})}
	_, err := New(display).Invoke(inConversation(), codeFlow(`read code; echo "rejected: $code" >&2; exit 3`))
	if err == nil || !strings.Contains(err.Error(), "rejected: BAD") {
		t.Fatalf("a failed sign-in was answered with %v", err)
	}
	if got := display.states(); len(got) == 0 || got[len(got)-1] != agent.SignInFailed {
		t.Fatalf("settled %v, want failed", got)
	}
}

// Every way the gateway can end a sign-in stops the process and is a hard stop
// for the model, each in words that say what happened.
func TestTheGatewayEndingASignInStopsIt(t *testing.T) {
	for _, tc := range []struct {
		answer agent.DisplayAnswer
		want   string
	}{
		{agent.DisplayAnswer{Outcome: agent.DisplayDenied, UserID: "UOWNER"}, "declined"},
		{agent.DisplayAnswer{Outcome: agent.DisplayDismissed}, "cancelled"},
		{agent.DisplayAnswer{Outcome: agent.DisplayNoConversation}, "only works inside a Slack conversation"},
		{agent.DisplayAnswer{Outcome: agent.DisplayUnavailable, Note: "UOWNER may no longer use this gateway"}, "may no longer use this gateway"},
	} {
		t.Run(string(tc.answer.Outcome), func(t *testing.T) {
			display := &fakeDisplay{answers: []agent.DisplayAnswer{tc.answer}}
			start := time.Now()
			_, err := New(display).Invoke(inConversation(), codeFlow("exec sleep 30"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("answered with %v, want it to say %q", err, tc.want)
			}
			if time.Since(start) > 10*time.Second {
				t.Fatal("the sign-in process was left running")
			}
			if got := display.states(); len(got) != 1 || got[0] != agent.SignInCancelled {
				t.Fatalf("settled %v, want cancelled so the node forgets it", got)
			}
		})
	}
}

func TestASignInThatNobodyFinishesTimesOut(t *testing.T) {
	display := &fakeDisplay{}
	tool := New(display)
	tool.timeout = 200 * time.Millisecond
	_, err := tool.Invoke(inConversation(), codeFlow("exec sleep 30"))
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("an unanswered sign-in was answered with %v", err)
	}
	if got := display.states(); len(got) != 1 || got[0] != agent.SignInTimedOut {
		t.Fatalf("settled %v, want timeout", got)
	}
}

func TestATurnThatCannotShowTheSignInStopsIt(t *testing.T) {
	display := &fakeDisplay{refuse: true}
	if _, err := New(display).Invoke(inConversation(), codeFlow("exec sleep 30")); err == nil {
		t.Fatal("a sign-in nobody could see was left waiting")
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

func marked(t *testing.T) (string, string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "ran")
	return marker, `touch "` + marker + `"; echo "Go to https://example.com/auth?x=1"; read code; [ "$code" = GOOD ]`
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// An agent-chosen command is shown to the owner and does not run, even in
// part, until they approve it; its link follows and the sign-in completes.
func TestACustomCommandRunsOnlyOnceItsOwnerApproves(t *testing.T) {
	marker, command := marked(t)
	display := &fakeDisplay{prompts: make(chan *agent.SignInPrompt, 1)}
	result := make(chan error, 1)
	go func() {
		_, err := New(display).Invoke(inConversation(), map[string]any{"tool": "vendor-mcp", "profile": "custom", "command": command, "needs_code": true})
		result <- err
	}()
	prompt := <-display.prompts
	if prompt.Request.Command != command || prompt.Request.URL != "" {
		t.Fatalf("raised %+v; want the exact command and no link", prompt.Request)
	}
	time.Sleep(300 * time.Millisecond)
	if exists(marker) {
		t.Fatal("the command ran before its owner approved it")
	}

	prompt.Answer <- agent.DisplayAnswer{Outcome: agent.DisplayApproved, UserID: "UOWNER"}
	deadline := time.Now().Add(10 * time.Second)
	for {
		display.mu.Lock()
		linked := len(display.links) == 1 && display.links[0] == "https://example.com/auth?x=1"
		display.mu.Unlock()
		if linked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the link never followed the approval")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !exists(marker) {
		t.Fatal("the approved command did not run")
	}
	prompt.Answer <- agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Code: "GOOD"}
	if err := <-result; err != nil {
		t.Fatalf("the approved sign-in failed: %v", err)
	}
	if got := display.states(); got[0] != agent.SignInReady || got[len(got)-1] != agent.SignInSuccess {
		t.Fatalf("settled %v, want ready first and success last", got)
	}
}

// Anything short of the owner's approval leaves the command unrun.
func TestACustomCommandThatIsNotApprovedNeverRuns(t *testing.T) {
	for name, tc := range map[string]struct {
		display *fakeDisplay
		timeout time.Duration
	}{
		"declined":                {display: &fakeDisplay{answers: []agent.DisplayAnswer{{Outcome: agent.DisplayDenied}}}},
		"withdrawn":               {display: &fakeDisplay{answers: []agent.DisplayAnswer{{Outcome: agent.DisplayDismissed}}}},
		"owner lost access":       {display: &fakeDisplay{answers: []agent.DisplayAnswer{{Outcome: agent.DisplayUnavailable, Note: "the person asked to sign in lost access"}}}},
		"a code, not an approval": {display: &fakeDisplay{answers: []agent.DisplayAnswer{{Outcome: agent.DisplayAnswered, Code: "GOOD"}}}, timeout: 300 * time.Millisecond},
		"nobody answered":         {display: &fakeDisplay{}, timeout: 300 * time.Millisecond},
		"nobody to ask":           {display: &fakeDisplay{refuse: true}},
	} {
		t.Run(name, func(t *testing.T) {
			marker, command := marked(t)
			tool := New(tc.display)
			if tc.timeout > 0 {
				tool.timeout = tc.timeout
			}
			if _, err := tool.Invoke(inConversation(), map[string]any{"tool": "vendor-mcp", "profile": "custom", "command": command, "needs_code": true}); err == nil {
				t.Fatal("an unapproved sign-in succeeded")
			}
			time.Sleep(200 * time.Millisecond)
			if exists(marker) {
				t.Fatal("the command ran without its owner's approval")
			}
		})
	}
}
