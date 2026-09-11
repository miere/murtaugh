package gateway

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"

	"github.com/miere/murtaugh/assets"
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
