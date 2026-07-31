package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/miere/murtaugh/internal/config"
	slacklib "github.com/miere/murtaugh/internal/slack/client"
	"github.com/miere/murtaugh/internal/slack/client/slacktest"
	askbroker "github.com/miere/murtaugh/internal/slack/interaction"
)

// allowAnyoneGateway builds a Gateway whose allowlist contains only UALICE00 and
// whose channel cache is COLD: the name behind C1 is discoverable solely through
// the read-through conversations.info fake. That coldness is the point — it is
// the state a brand-new channel is in, and the state in which the old
// nameFor-only check silently dropped the first message.
func allowAnyoneGateway(t *testing.T, channelName string, rules config.ChannelRules) (*Gateway, *fakeChatSessions, *countingCanvasInfo) {
	t.Helper()
	sessions := &fakeChatSessions{}
	info := &countingCanvasInfo{channel: channelNamed(channelName)}
	app := &Gateway{
		chat:         NewChatHandler(&fakeStreamAPI{}, map[string]ChatSessionManager{"default": sessions}, func(ChatRequest) ChatRoute { return ChatRoute{Agent: "default", ReplyOnThread: true} }, time.Hour, 1, nil),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:          config.AccessConfig{AllowedUsers: []string{"UALICE00"}},
		chatChannels: rules,
		channelCache: newChannelNameCache(&fakeChannelDirectory{}, info, nil, time.Second, cacheTestLogger()),
	}
	return app, sessions, info
}

func mentionEvent(user, channel, ts string) socketmode.Event {
	return socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: slackevents.EventsAPIEvent{
		TeamID:     "T1",
		InnerEvent: slackevents.EventsAPIInnerEvent{Type: string(slackevents.AppMention), Data: &slackevents.AppMentionEvent{User: user, Channel: channel, Text: "<@UBOT> hello", TimeStamp: ts}},
	}}
}

// waitForPrompt polls until a prompt is recorded, returning "" if none arrives.
// Admission is asynchronous (it may do a Slack round-trip), so a positive
// assertion has to wait rather than read once.
func waitForPrompt(t *testing.T, sessions *fakeChatSessions) string {
	t.Helper()
	for i := 0; i < 100; i++ {
		if got := sessions.promptText(); got != "" {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ""
}

// TestAppMentionAllowAnyoneAdmitsStrangerOnColdCache is the regression this
// feature exists for: a user who is NOT on the global allowlist mentions the bot
// in a channel matched by an allow_anyone rule, and the channel's name has never
// been cached. The message must be answered — resolved read-through — not
// dropped and left to a later refresh.
func TestAppMentionAllowAnyoneAdmitsStrangerOnColdCache(t *testing.T) {
	app, sessions, info := allowAnyoneGateway(t, "nc-platform", config.ChannelRules{
		{Match: "nc-*", Agent: "default", AllowAnyone: true},
	})
	if _, cached := app.channelCache.nameFor("C1"); cached {
		t.Fatal("precondition: the cache must be cold for this test to mean anything")
	}

	app.handleEventsAPI(mentionEvent("UEVIL000", "C1", "123.4"))

	if got := waitForPrompt(t, sessions); got != "hello" {
		t.Fatalf("expected the stranger's mention to be answered, got prompt %q", got)
	}
	if n := info.calls.Load(); n != 1 {
		t.Fatalf("conversations.info called %d times, want exactly 1 read-through resolve", n)
	}
}

// TestAppMentionAllowAnyoneIgnoresStrangerInUnflaggedChannel is the other half:
// the same stranger, the same cold cache, but the channel's rule does not opt
// in. Fail-closed still holds.
func TestAppMentionAllowAnyoneIgnoresStrangerInUnflaggedChannel(t *testing.T) {
	app, sessions, _ := allowAnyoneGateway(t, "mt-ops", config.ChannelRules{
		{Match: "nc-*", Agent: "default", AllowAnyone: true},
		{Match: "mt-*", Agent: "default"},
	})

	app.handleEventsAPI(mentionEvent("UEVIL000", "C1", "123.4"))

	time.Sleep(100 * time.Millisecond)
	if got := sessions.promptText(); got != "" {
		t.Fatalf("expected the stranger's mention to be ignored, got prompt %q", got)
	}
}

// TestAppMentionAllowAnyoneNarrowRuleWinsOverBroadOne pins the first-match-wins
// safety property end to end: a closed rule listed ABOVE a broad allow_anyone
// one keeps its channel closed.
func TestAppMentionAllowAnyoneNarrowRuleWinsOverBroadOne(t *testing.T) {
	app, sessions, _ := allowAnyoneGateway(t, "nc-secrets", config.ChannelRules{
		{Match: "nc-secrets", Agent: "default"},
		{Match: "nc-*", Agent: "default", AllowAnyone: true},
	})

	app.handleEventsAPI(mentionEvent("UEVIL000", "C1", "123.4"))

	time.Sleep(100 * time.Millisecond)
	if got := sessions.promptText(); got != "" {
		t.Fatalf("expected the narrow closed rule to win, got prompt %q", got)
	}
}

// TestAllowlistedUserNeedsNoChannelRule guards the common path: a user on the
// global allowlist is admitted in a channel with no matching rule at all.
func TestAllowlistedUserNeedsNoChannelRule(t *testing.T) {
	app, sessions, _ := allowAnyoneGateway(t, "random", nil)

	app.handleEventsAPI(mentionEvent("UALICE00", "C1", "123.4"))

	if got := waitForPrompt(t, sessions); got != "hello" {
		t.Fatalf("expected the allowlisted user to be answered, got prompt %q", got)
	}
}

// TestAllowAnyoneStillRequiresMention: opening a channel for chat must not also
// waive the @mention requirement, or the bot would answer every message posted
// there. The author is not in any no-mention list, so a plain message is ignored
// even though the channel is open.
func TestAllowAnyoneStillRequiresMention(t *testing.T) {
	app, sessions, _ := allowAnyoneGateway(t, "nc-platform", config.ChannelRules{
		{Match: "nc-*", Agent: "default", AllowAnyone: true},
	})

	app.handleEventsAPI(socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: slackevents.EventsAPIEvent{
		TeamID:     "T1",
		InnerEvent: slackevents.EventsAPIInnerEvent{Type: string(slackevents.Message), Data: &slackevents.MessageEvent{User: "UEVIL000", Channel: "C1", Text: "just chatting", TimeStamp: "123.4"}},
	}})

	time.Sleep(100 * time.Millisecond)
	if got := sessions.promptText(); got != "" {
		t.Fatalf("expected a plain message to still need an @mention, got prompt %q", got)
	}
}

// TestAllowAnyoneDoesNotReachDMs: the waiver is channel-scoped. Even with a rule
// whose match is the DM's own conversation ID, a stranger's DM stays denied,
// because handleDirectMessage consults the global allowlist directly.
func TestAllowAnyoneDoesNotReachDMs(t *testing.T) {
	app, sessions, _ := allowAnyoneGateway(t, "nc-platform", config.ChannelRules{
		{Match: "D1", Agent: "default", AllowAnyone: true},
	})

	app.handleEventsAPI(socketmode.Event{Type: socketmode.EventTypeEventsAPI, Data: slackevents.EventsAPIEvent{
		TeamID:     "T1",
		InnerEvent: slackevents.EventsAPIInnerEvent{Type: string(slackevents.Message), Data: &slackevents.MessageEvent{User: "UEVIL000", Channel: "D1", ChannelType: "im", Text: "hello", TimeStamp: "123.4"}},
	}})

	time.Sleep(100 * time.Millisecond)
	if got := sessions.promptText(); got != "" {
		t.Fatalf("expected a stranger's DM to stay denied, got prompt %q", got)
	}
}

// TestAllowAnyoneDoesNotReachSlashCommands: an open channel must not hand a
// stranger the slash surface — that is where troubleshoot lives, and its bundle
// can carry sensitive data.
func TestAllowAnyoneDoesNotReachSlashCommands(t *testing.T) {
	handler := &recordingSlashHandler{}
	app := &Gateway{
		handler:      handler,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:          config.AccessConfig{AllowedUsers: []string{"UALICE00"}},
		chatChannels: config.ChannelRules{{Match: "C1", Agent: "default", AllowAnyone: true}},
	}
	app.handleSlashCommand(context.Background(), socketmode.Event{
		Type: socketmode.EventTypeSlashCommand,
		Data: slack.SlashCommand{Command: "/murtaugh", UserID: "UEVIL000", ChannelID: "C1", Text: "troubleshoot"},
	})
	if handler.calls != 0 {
		t.Fatalf("expected the slash surface to stay allowlist-only, got %d calls", handler.calls)
	}
}

// TestAllowAnyoneDoesNotReachWorkflowRules: a channel guest's authority stops at
// the agent's own prompts. Workflow rules are operator-configured and can run
// commands or delegate to other agents, so their blast radius is not bounded by
// the channel agent's toolset — they stay allowlist-only.
func TestAllowAnyoneDoesNotReachWorkflowRules(t *testing.T) {
	wf := &recordingWorkflow{}
	app := &Gateway{
		workflow:     wf,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:          config.AccessConfig{AllowedUsers: []string{"UALICE00"}},
		chatChannels: config.ChannelRules{{Match: "C1", Agent: "default", AllowAnyone: true}},
	}
	app.handleInteractive(socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: slack.InteractionCallback{
			Type:    slack.InteractionTypeBlockActions,
			User:    slack.User{ID: "UEVIL000"},
			Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C1"}}},
		},
	})
	time.Sleep(100 * time.Millisecond)
	if calls, _ := wf.stats(); calls != 0 {
		t.Fatalf("expected workflow rules to stay allowlist-only, got %d calls", calls)
	}
}

// TestAllowAnyoneDoesNotReachWorkflowRulesForOutsiders is the plain fail-closed
// case: a stranger clicking in a channel with NO opening rule reaches nothing.
func TestAllowAnyoneDoesNotReachWorkflowRulesForOutsiders(t *testing.T) {
	wf := &recordingWorkflow{}
	app := &Gateway{
		workflow:     wf,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:          config.AccessConfig{AllowedUsers: []string{"UALICE00"}},
		chatChannels: config.ChannelRules{{Match: "nc-*", Agent: "default", AllowAnyone: true}},
	}
	app.handleInteractive(socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: slack.InteractionCallback{
			Type:    slack.InteractionTypeBlockActions,
			User:    slack.User{ID: "UEVIL000"},
			Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: "C9"}}},
		},
	})
	time.Sleep(100 * time.Millisecond)
	if calls, _ := wf.stats(); calls != 0 {
		t.Fatalf("expected an outsider's click to be denied outright, got %d calls", calls)
	}
}

// brokerClick builds the block_actions payload Slack delivers when someone
// clicks a broker prompt button (an `ask` option or a tool-approval choice)
// carrying correlation id corr.
func brokerClick(user, channelID, corr string) socketmode.Event {
	value, _ := json.Marshal(map[string]string{"id": "approve", "label": "Allow"})
	return socketmode.Event{
		Type: socketmode.EventTypeInteractive,
		Data: slack.InteractionCallback{
			Type:    slack.InteractionTypeBlockActions,
			User:    slack.User{ID: user},
			Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: channelID}}},
			ActionCallback: slack.ActionCallbacks{
				BlockActions: []*slack.BlockAction{{
					ActionID: askbroker.ActionPrefix + corr + ":approve",
					BlockID:  askbroker.BlockID,
					Value:    string(value),
				}},
			},
		},
	}
}

// approvalGateway builds a Gateway with a live broker over a fake Slack API, so
// a test can post a real prompt and resolve it through handleInteractive.
func approvalGateway(t *testing.T, rules config.ChannelRules) (*Gateway, *signalingSlackAPI) {
	t.Helper()
	sig := &signalingSlackAPI{
		FakeAPI: &slacktest.FakeAPI{PostResult: slacklib.PostMessageResult{Channel: "C1", TS: "1700.1"}},
		posted:  make(chan slacklib.PostMessageParams, 1),
	}
	app := &Gateway{
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		interactions: askbroker.NewWith(slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return sig, nil })),
		cfg:          config.AccessConfig{AllowedUsers: []string{"UALICE00"}},
		chatChannels: rules,
		channelCache: newChannelNameCache(&fakeChannelDirectory{}, &countingCanvasInfo{channel: channelNamed("nc-platform")}, nil, time.Second, cacheTestLogger()),
	}
	return app, sig
}

// askApprove posts a permission-style prompt and returns a channel carrying the
// option the user eventually picks ("" if the prompt is never answered).
func askApprove(t *testing.T, app *Gateway, sig *signalingSlackAPI) (<-chan string, string) {
	t.Helper()
	out := make(chan string, 1)
	go func() {
		d, err := app.interactions.Ask(context.Background(), askbroker.Destination{ChannelID: "C1"}, askbroker.PromptSpec{
			Title:    ":lock: Permission needed",
			Question: "The agent wants to use the `bash` tool. Allow?",
			Options:  []askbroker.Option{{ID: "approve", Label: "Allow"}, {ID: "deny", Label: "Deny"}},
			Timeout:  5 * time.Second,
		})
		if err != nil {
			out <- ""
			return
		}
		out <- d.OptionID
	}()
	posted := <-sig.posted
	return out, corrFromBlocks(t, posted.Blocks)
}

// TestAllowAnyoneGuestCanAnswerApproval is the point of routing approvals through
// the channel's authority: a guest who may drive a turn in an opened channel can
// also answer the approval that turn raises. Without this their turn would stall
// on a button they are not allowed to click.
func TestAllowAnyoneGuestCanAnswerApproval(t *testing.T) {
	app, sig := approvalGateway(t, config.ChannelRules{{Match: "nc-*", Agent: "coder", AllowAnyone: true}})
	out, corr := askApprove(t, app, sig)

	app.handleInteractive(brokerClick("UEVIL000", "C1", corr))

	select {
	case got := <-out:
		if got != "approve" {
			t.Fatalf("prompt resolved to %q, want the guest's choice %q", got, "approve")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the guest's approval never reached the blocked turn")
	}
}

// TestAllowAnyoneOutsiderCannotAnswerApproval: the same click from a channel with
// no opening rule must not resolve the prompt.
func TestAllowAnyoneOutsiderCannotAnswerApproval(t *testing.T) {
	app, sig := approvalGateway(t, config.ChannelRules{{Match: "mt-*", Agent: "admin"}})
	out, corr := askApprove(t, app, sig)

	app.handleInteractive(brokerClick("UEVIL000", "C1", corr))

	select {
	case got := <-out:
		t.Fatalf("an outsider's click resolved the prompt to %q, want it ignored", got)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestAllowlistedUserCanAnswerApprovalAnywhere: the allowlist path is unchanged
// and needs no channel rule at all.
func TestAllowlistedUserCanAnswerApprovalAnywhere(t *testing.T) {
	app, sig := approvalGateway(t, nil)
	out, corr := askApprove(t, app, sig)

	app.handleInteractive(brokerClick("UALICE00", "C1", corr))

	select {
	case got := <-out:
		if got != "approve" {
			t.Fatalf("prompt resolved to %q, want %q", got, "approve")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the allowlisted user's approval never reached the blocked turn")
	}
}
