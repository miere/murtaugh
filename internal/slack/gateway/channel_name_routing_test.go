package gateway

import (
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/miere/murtaugh/internal/config"
)

// The resolver reads the channel cache rather than calling Slack, because it runs
// on the socket goroutine.
func TestTheResolverPutsTheChannelNameOnTheRoute(t *testing.T) {
	app := New(config.Config{
		OAuth: config.OAuthConfig{AppToken: "xapp-test", BotToken: "xoxb-test"},
		Chat:  config.ChatConfig{Enabled: true, Defaults: config.ChatDefaults{Agent: "default"}},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil, nil, nil)
	if app.chat == nil || app.chat.resolver == nil {
		t.Fatal("the gateway built no chat resolver")
	}
	app.channelCache.memoize("C1", "nc-releases")

	route := app.chat.resolver(ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1"})
	if route.ChannelName != "nc-releases" {
		t.Fatalf("the resolved route carries channel name %q; a node's `nc-*` claim can never match", route.ChannelName)
	}
}

type coldCacheResolver struct {
	calls atomic.Int32
	name  string
}

func (r *coldCacheResolver) resolve(ChatRequest) ChatRoute {
	route := ChatRoute{Agent: "default", ReplyOnThread: true}
	if r.calls.Add(1) > 1 {
		route.ChannelName = r.name
	}
	return route
}

// Delegation matches node channel claims by name, so a name lost on either link
// routes a brand-new channel as though nobody claimed it.
func TestTheChannelNameReachesTheAgentsSessionMetadata(t *testing.T) {
	api := &fakeStreamAPI{}
	fake := &fakeChatSessions{}
	resolver := &coldCacheResolver{name: "nc-releases"}
	app := &Gateway{
		chat: NewChatHandler(api, map[string]ChatSessionManager{"default": fake},
			resolver.resolve, time.Hour, 1, nil),
		inFlight: NewInFlightRegistry(),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:      config.AccessConfig{AllowedUsers: []string{"U1"}},
	}
	app.handleEventsAPI(socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: slackevents.EventsAPIEvent{
		TeamID: "T1",
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: string(slackevents.AppMention),
			Data: &slackevents.AppMentionEvent{User: "U1", Channel: "C1", Text: "<@UBOT> hello", TimeStamp: "123.4"},
		},
	}})

	deadline := time.After(5 * time.Second)
	for fake.promptText() == "" {
		select {
		case <-deadline:
			t.Fatal("the mention never reached the agent")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if got := fake.metadata().ChannelName; got != "nc-releases" {
		t.Fatalf("the agent's session metadata names channel %q; delegation matches claims against this, and every name glob stops matching without it", got)
	}
	if resolver.calls.Load() < 2 {
		t.Fatal("the route was resolved once; dispatchTurn must re-resolve it off the socket goroutine or a cold cache is never corrected")
	}
	if fake.key.ChannelID != "C1" || fake.key.ThreadTS != "123.4" {
		t.Fatalf("the conversation key diverged: %+v", fake.key)
	}
}
