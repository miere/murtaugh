package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/slack/authcard"
	slacklib "github.com/miere/murtaugh/internal/slack/client"
	"github.com/miere/murtaugh/internal/slack/client/slacktest"
)

// A submission from an owner who lost access must still reach the flow, or the
// node's sign-in would run on for nobody.
func TestASignInSubmissionFromSomeoneWhoLostAccessStopsTheSignIn(t *testing.T) {
	api := &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "D1", TS: "1.1"}}
	flow := authcard.New(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return api, nil }),
		authcard.NewRenderer("", assets.FS), "UADMIN00", nil)
	app := &Gateway{
		auth:   flow,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:    config.AccessConfig{AdminUser: "UADMIN00", AllowedUsers: []string{"UOWNER00"}},
	}
	flow.SetAuthorised(func(userID string) bool { return app.access().IsAllowedUser(userID) })

	card, err := flow.Show(context.Background(), authcard.Showing{ToolName: "gcp-mcp", ProfileName: "gcloud", URL: "https://accounts.example.com", NeedsCode: true, Recipient: "UOWNER00"})
	if err != nil {
		t.Fatalf("Show: %v", err)
	}
	defer card.Settle(authcard.StateCancelled, "")
	match := regexp.MustCompile(`murtaugh_auth:([0-9a-f]+):`).FindSubmatch(api.Posted[0].Blocks)
	if match == nil {
		t.Fatalf("the card carries no correlation id:\n%s", api.Posted[0].Blocks)
	}

	app.setAccess(func(access *config.AccessConfig) { access.AllowedUsers = nil })
	app.handleInteractive(socketmode.Event{Type: socketmode.EventTypeInteractive, Data: slack.InteractionCallback{
		Type: slack.InteractionTypeViewSubmission,
		User: slack.User{ID: "UOWNER00"},
		View: slack.View{
			CallbackID:      authcard.ModalCallbackID,
			PrivateMetadata: string(match[1]),
			State: &slack.ViewState{Values: map[string]map[string]slack.BlockAction{
				"murtaugh_auth_code_block": {"murtaugh_auth_code_input": {Value: "4/0Ab-code"}},
			}},
		},
	}})

	select {
	case reply := <-card.Replies():
		if reply.Kind != authcard.ReplyRefused || reply.Code != "" {
			t.Fatalf("the submission came back as %+v, want a refusal carrying no code", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the submission was dropped before the flow could refuse it; the sign-in would run on")
	}
}

func TestASignInWithNoConversationIsDrawnInTheOwnersDMAlone(t *testing.T) {
	api := &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "DOWNER", TS: "1.1"}, DMFor: map[string]string{"UOWNER00": "DOWNER"}}
	flow := authcard.New(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return api, nil }),
		authcard.NewRenderer("", assets.FS), "UADMIN00", nil)
	flow.SetAuthorised(func(userID string) bool { return userID == "UOWNER00" })
	var hooks agentruntime.Hooks
	New(config.Config{
		OAuth:  config.OAuthConfig{AppToken: "xapp-test", BotToken: "xoxb-test"},
		Access: config.AccessConfig{AdminUser: "UADMIN00", AllowedUsers: []string{"UOWNER00"}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, flow, nil, func(h agentruntime.Hooks) agentruntime.Runtime {
		hooks = h
		return agentruntime.Runtime{}
	})
	if hooks.SignIn == nil {
		t.Fatal("the gateway gave the runtime nowhere to draw a sign-in with no conversation")
	}

	prompt := &agent.SignInPrompt{
		Request: agent.SignInRequest{Tool: "gcp-mcp", Profile: "gcloud", URL: "https://accounts.example.com"},
		Owner:   "UOWNER00",
		Answer:  make(chan agent.DisplayAnswer, 2),
	}
	settled := make(chan agent.SignInSettled, 1)
	settled <- agent.SignInSettled{Prompt: prompt, State: agent.SignInSuccess}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var shownErr error = errors.New("never shown")
	hooks.SignIn(ctx, prompt, settled, func(err error) { shownErr = err })
	if shownErr != nil {
		t.Fatalf("the card was reported as not shown: %v", shownErr)
	}

	if len(api.Posted) != 1 {
		t.Fatalf("posted %d messages, want the one card in the owner's DM", len(api.Posted))
	}
	for _, p := range api.Posted {
		if p.ChannelID != "DOWNER" || p.ThreadTS != "" {
			t.Fatalf("posted to %q/%q; a sign-in with no conversation belongs in the owner's DM alone", p.ChannelID, p.ThreadTS)
		}
	}
	for _, u := range api.Updated {
		if u.ChannelID != "DOWNER" {
			t.Fatalf("updated %q; a sign-in with no conversation belongs in the owner's DM alone", u.ChannelID)
		}
	}
}
