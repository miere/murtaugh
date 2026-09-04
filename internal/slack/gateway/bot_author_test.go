package gateway

import (
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// botMentionEvent is an app_mention written by another app: Slack fills in
// bot_id alongside the bot user's own `user` id. Riggs tagging Murtaugh to ask
// for a PR review arrives exactly like this.
func botMentionEvent(team, channel, user, botID, ts string) socketmode.Event {
	return socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: slackevents.EventsAPIEvent{
		TeamID: team,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: string(slackevents.AppMention),
			Data: &slackevents.AppMentionEvent{User: user, BotID: botID, Channel: channel, Text: "<@UBOT> review this PR", TimeStamp: ts},
		},
	}}
}

// botChannelMessageEvent is a plain (non-mention) channel message written by an
// app. The shared channelMessageEvent helper leaves bot_id empty, so this path
// needs its own builder to exercise authorship at all.
func botChannelMessageEvent(team, channel, user, botID, text, ts string) socketmode.Event {
	return socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: slackevents.EventsAPIEvent{
		TeamID: team,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Type: string(slackevents.Message),
			Data: &slackevents.MessageEvent{User: user, BotID: botID, Channel: channel, ChannelType: "channel", Text: text, TimeStamp: ts},
		},
	}}
}

// withSelf stamps a resolved auth.test identity onto a test gateway, which is
// what the constructor does in production.
func withSelf(app *Gateway, userID, botID string) *Gateway {
	app.selfUserID, app.selfBotID = userID, botID
	return app
}

// The regression this file exists for. Riggs (URIGGS00/BRIGGS00) is on
// allowed_users; its @mention must start a chat exactly like a human's.
func TestAppMentionFromAnotherBotOnTheAllowlistStartsChat(t *testing.T) {
	cache := primedCache(t, map[string]string{"C1": "general"})
	app, sessions := newNoMentionGateway(nil, nil, []string{"URIGGS00"}, cache)
	withSelf(app, "UMURTAUGH", "BMURTAUGH")

	app.handleEventsAPI(botMentionEvent("T1", "C1", "URIGGS00", "BRIGGS00", "200.1"))

	if got := waitForPrompts(sessions, 1); got != 1 {
		t.Fatalf("expected an allowlisted bot's @mention to start exactly one chat, got %d", got)
	}
}

// The allowlist is the only gate. A bot that is not on it is refused for the
// same reason an unlisted human is — not for being a bot.
func TestAppMentionFromUnlistedBotIsIgnored(t *testing.T) {
	cache := primedCache(t, map[string]string{"C1": "general"})
	app, sessions := newNoMentionGateway(nil, nil, []string{"U1"}, cache)
	withSelf(app, "UMURTAUGH", "BMURTAUGH")

	app.handleEventsAPI(botMentionEvent("T1", "C1", "USTRANGER", "BSTRANGER", "201.1"))

	if got := waitForPrompts(sessions, 1); got != 0 {
		t.Fatalf("expected an unlisted bot's @mention to be ignored, got %d prompts", got)
	}
}

// The loop guard: our own reply carrying "<@self>" comes back as an
// app_mention. Answering it would produce another one, without end.
func TestAppMentionWrittenByOurselvesIsIgnored(t *testing.T) {
	cache := primedCache(t, map[string]string{"C1": "general"})
	// Pathological but the point: even if our own id were on the allowlist, the
	// self check runs first.
	app, sessions := newNoMentionGateway(nil, nil, []string{"UMURTAUGH"}, cache)
	withSelf(app, "UMURTAUGH", "BMURTAUGH")

	app.handleEventsAPI(botMentionEvent("T1", "C1", "UMURTAUGH", "BMURTAUGH", "202.1"))

	if got := waitForPrompts(sessions, 1); got != 0 {
		t.Fatalf("expected a self-authored @mention to be ignored, got %d prompts", got)
	}
}

// Some Slack surfaces carry bot_id without a matching `user`. The self check
// has to recognise us by either half of the identity.
func TestSelfAuthoredIsRecognisedByBotIDAlone(t *testing.T) {
	cache := primedCache(t, map[string]string{"C1": "general"})
	app, sessions := newNoMentionGateway(nil, nil, []string{"U1"}, cache)
	withSelf(app, "UMURTAUGH", "BMURTAUGH")

	app.handleEventsAPI(botMentionEvent("T1", "C1", "", "BMURTAUGH", "203.1"))

	if got := waitForPrompts(sessions, 1); got != 0 {
		t.Fatalf("expected a self-authored @mention matched by bot_id to be ignored, got %d prompts", got)
	}
}

// auth.test failed at startup, so we cannot tell our own messages from anyone
// else's. Fall back to the old blanket refusal rather than risk a loop.
func TestUnknownSelfIdentityRefusesEveryBotAuthoredMention(t *testing.T) {
	cache := primedCache(t, map[string]string{"C1": "general"})
	app, sessions := newNoMentionGateway(nil, nil, []string{"URIGGS00"}, cache)
	// No withSelf: both identity fields stay empty.

	app.handleEventsAPI(botMentionEvent("T1", "C1", "URIGGS00", "BRIGGS00", "204.1"))

	if got := waitForPrompts(sessions, 1); got != 0 {
		t.Fatalf("expected an unknown self identity to refuse bot-authored mentions, got %d prompts", got)
	}
}

// The fallback must not take humans down with it: with no self identity, a
// human @mention still has no bot_id and still gets answered.
func TestUnknownSelfIdentityStillAnswersHumans(t *testing.T) {
	cache := primedCache(t, map[string]string{"C1": "general"})
	app, sessions := newNoMentionGateway(nil, nil, []string{"U1"}, cache)

	app.handleEventsAPI(botMentionEvent("T1", "C1", "U1", "", "205.1"))

	if got := waitForPrompts(sessions, 1); got != 1 {
		t.Fatalf("expected a human @mention to be answered without a resolved self identity, got %d prompts", got)
	}
}

// A plain (non-mention) channel message from a bot on the no-mention list is
// answered on the same terms as a human's — the bot check is gone from that
// path too, leaving only the waiver.
func TestChannelMessageFromListedBotStartsChatWithoutMention(t *testing.T) {
	cache := primedCache(t, map[string]string{"C1": "general"})
	app, sessions := newNoMentionGateway([]string{"URIGGS00"}, nil, []string{"URIGGS00"}, cache)
	withSelf(app, "UMURTAUGH", "BMURTAUGH")

	app.handleEventsAPI(botChannelMessageEvent("T1", "C1", "URIGGS00", "BRIGGS00", "build is green", "206.1"))

	if got := waitForPrompts(sessions, 1); got != 1 {
		t.Fatalf("expected a waived bot's plain channel message to start a chat, got %d", got)
	}
}

// Our own plain channel message is never answered, waiver or not.
func TestChannelMessageWrittenByOurselvesIsIgnored(t *testing.T) {
	cache := primedCache(t, map[string]string{"C1": "general"})
	app, sessions := newNoMentionGateway([]string{"UMURTAUGH"}, nil, []string{"UMURTAUGH"}, cache)
	withSelf(app, "UMURTAUGH", "BMURTAUGH")

	app.handleEventsAPI(botChannelMessageEvent("T1", "C1", "UMURTAUGH", "BMURTAUGH", "thinking out loud", "207.1"))

	if got := waitForPrompts(sessions, 1); got != 0 {
		t.Fatalf("expected our own channel message to be ignored, got %d prompts", got)
	}
}

// An app named in allowed_users has to be writable as a handle, which is how an
// operator reads it out of the Slack member list.
func TestResolveUserIDsResolvesBotHandles(t *testing.T) {
	api := &fakeUserDirectory{users: []slack.User{
		{ID: "UALICE00", Name: "alice"},
		{ID: "URIGGS00", Name: "riggs", IsBot: true},
	}}

	ids, err := resolveUserIDs(t.Context(), api, []string{"@riggs", "alice"})
	if err != nil {
		t.Fatalf("resolveUserIDs returned error: %v", err)
	}
	if len(ids) != 2 || ids[0] != "URIGGS00" || ids[1] != "UALICE00" {
		t.Fatalf("unexpected resolution: %#v", ids)
	}
}

// Deleted accounts stay skipped: dropping the IsBot half of that condition must
// not quietly resurrect a departed member.
func TestResolveUserIDsStillSkipsDeletedUsers(t *testing.T) {
	api := &fakeUserDirectory{users: []slack.User{
		{ID: "UGONE000", Name: "riggs", Deleted: true},
	}}

	if _, err := resolveUserIDs(t.Context(), api, []string{"@riggs"}); err == nil {
		t.Fatal("expected a deleted user's handle to fail resolution")
	}
}
