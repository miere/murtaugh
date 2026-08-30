package gateway

import (
	"context"
	"log/slog"
	"testing"

	"github.com/slack-go/slack"
)

type fakeSlackAPI struct {
	users       []slack.User
	openedUsers []string
	postChannel string
	postOptions int
}

func (f *fakeSlackAPI) GetUsersContext(context.Context, ...slack.GetUsersOption) ([]slack.User, error) {
	return f.users, nil
}

func (f *fakeSlackAPI) OpenConversationContext(_ context.Context, params *slack.OpenConversationParameters) (*slack.Channel, bool, bool, error) {
	f.openedUsers = append(f.openedUsers, params.Users...)
	return &slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "DADMIN"}}}, false, false, nil
}

func (f *fakeSlackAPI) PostMessageContext(_ context.Context, channelID string, options ...slack.MsgOption) (string, string, error) {
	f.postChannel = channelID
	f.postOptions = len(options)
	return channelID, "1717450123.000100", nil
}

func TestSlackStartupNotifierSendsPingToAdminHandle(t *testing.T) {
	api := &fakeSlackAPI{users: []slack.User{{ID: "UADMIN", Name: "admin"}}}
	// No card renderer or raw-blocks client: the notifier takes its text
	// fallback, which is what this test's fake can observe.
	notifier, err := NewSlackStartupNotifier(api, "@admin", nil, nil, slog.Default())
	if err != nil {
		t.Fatalf("NewSlackStartupNotifier returned error: %v", err)
	}
	if err := notifier.NotifyStartup(context.Background()); err != nil {
		t.Fatalf("NotifyStartup returned error: %v", err)
	}
	if len(api.openedUsers) != 1 || api.openedUsers[0] != "UADMIN" {
		t.Fatalf("expected DM to UADMIN, got %#v", api.openedUsers)
	}
	if api.postChannel != "DADMIN" || api.postOptions == 0 {
		t.Fatalf("expected startup ping in DADMIN with message options, got channel=%q options=%d", api.postChannel, api.postOptions)
	}
}

// The startup greeting is a passing remark, so it posts as the discreet
// one-line notice rather than a container card — through the ordinary client,
// since a context block needs no raw-blocks passthrough.
func TestSlackStartupNotifierPostsNoticeWhenWired(t *testing.T) {
	api := &fakeSlackAPI{users: []slack.User{{ID: "UADMIN", Name: "admin"}}}
	cardAPI := &fakeAlertAPI{}
	notifier, err := NewSlackStartupNotifier(api, "@admin", testAlertCards(), cardAPI, slog.Default())
	if err != nil {
		t.Fatalf("NewSlackStartupNotifier returned error: %v", err)
	}
	if err := notifier.NotifyStartup(context.Background()); err != nil {
		t.Fatalf("NotifyStartup returned error: %v", err)
	}
	if len(cardAPI.posts) != 0 {
		t.Fatalf("the startup greeting was posted as a card, got %d card post(s)", len(cardAPI.posts))
	}
	if api.postChannel != "DADMIN" {
		t.Fatalf("expected the notice in DADMIN, got %q", api.postChannel)
	}
}
