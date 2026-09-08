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

// The channel NAME's journey from the Slack side to the agent, which delegation
// (#196) matches a runtime node's claims against.
//
// A node's claim is an exact channel id, an exact channel name, or a glob over
// the name — and #170's entire worked example (`nc-*`, `review-*`) is the last
// two. So an id alone matches one shape in three: with this wiring broken every
// claim except a bare id stops matching, every election falls through to step 4,
// and the whole fleet round robins. Nothing errors, because an unclaimed channel
// is a legal state, which is why each of the three links is pinned here.

// The resolver puts the name on the route. It reads the in-memory channel cache
// rather than calling Slack, because it runs on the socket goroutine.
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

// coldCacheResolver answers with no name the first time and with the real name
// afterwards — a brand-new channel, whose name startChat cannot know and
// dispatchTurn resolves.
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

// And the name reaches the agent's session metadata, corrected on the way.
//
// Two links in one assertion, because the cold cache is the state that needs
// both: startChat resolves the route off the socket goroutine and gets nothing,
// dispatchTurn re-resolves it after a bounded conversations.info and gets the
// name, and Handle is what puts it on the metadata. Drop either the correction
// or the metadata field and a brand-new channel is delegated as though nobody
// claimed it — which is exactly the channel somebody has just created for a
// project and pointed a node at.
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
	// The conversation key is unaffected, which is why correcting the name in
	// dispatchTurn is safe: it is built from ReplyOnThread and nothing else the
	// resolver returns.
	if fake.key.ChannelID != "C1" || fake.key.ThreadTS != "123.4" {
		t.Fatalf("the conversation key diverged: %+v", fake.key)
	}
}
