package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

type fakeBackfillAPI struct {
	replies []slack.Message
	users   map[string]*slack.User
	repErr  error
	calls   int
}

func (f *fakeBackfillAPI) GetConversationRepliesContext(_ context.Context, _ *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	f.calls++
	return f.replies, false, "", f.repErr
}

func (f *fakeBackfillAPI) GetUserInfoContext(_ context.Context, user string) (*slack.User, error) {
	if u, ok := f.users[user]; ok {
		return u, nil
	}
	return nil, errors.New("user not found")
}

func msg(ts, user, text string) slack.Message {
	return slack.Message{Msg: slack.Msg{Timestamp: ts, User: user, Text: text}}
}

func userWithDisplayName(name string) *slack.User {
	u := &slack.User{}
	u.Profile.DisplayName = name
	return u
}

func TestBackfillRendersThreadExcludingTriggerAndTagsBot(t *testing.T) {
	api := &fakeBackfillAPI{
		replies: []slack.Message{
			msg("1700000000.000200", "UBOT", "on it"), // out of order on purpose
			msg("1700000000.000100", "U1", "hi"),
			msg("1700000000.000300", "U1", "thanks"), // the triggering message
		},
		users: map[string]*slack.User{
			"U1":   userWithDisplayName("miere"),
			"UBOT": userWithDisplayName("Murtaughbot"),
		},
	}
	b := NewThreadBackfiller(api, "UBOT", nil)

	out, err := b.Backfill(context.Background(), "C1", "1700000000.000100", "1700000000.000300")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(out, "@miere: hi") {
		t.Fatalf("expected human line with resolved name, got:\n%s", out)
	}
	if !strings.Contains(out, "@Murtaughbot (you): on it") {
		t.Fatalf("expected bot line tagged (you), got:\n%s", out)
	}
	if strings.Contains(out, "thanks") {
		t.Fatalf("triggering message should be excluded, got:\n%s", out)
	}
	if !strings.Contains(out, "In this workspace you post as @Murtaughbot.") {
		t.Fatalf("preamble should name the bot alias once, got:\n%s", out)
	}
	if !strings.HasPrefix(out, "<thread-transcript>") || !strings.HasSuffix(out, "</thread-transcript>") {
		t.Fatalf("transcript should be framed, got:\n%s", out)
	}
	// Oldest-first: the human's "hi" must precede the bot's "on it".
	if strings.Index(out, "hi") > strings.Index(out, "on it") {
		t.Fatalf("messages should be oldest-first, got:\n%s", out)
	}
}

func TestBackfillEmptyWhenOnlyTriggeringMessage(t *testing.T) {
	api := &fakeBackfillAPI{
		replies: []slack.Message{msg("1700000000.000300", "U1", "first post in a brand-new thread")},
		users:   map[string]*slack.User{"U1": userWithDisplayName("miere")},
	}
	b := NewThreadBackfiller(api, "UBOT", nil)

	out, err := b.Backfill(context.Background(), "C1", "1700000000.000300", "1700000000.000300")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "" {
		t.Fatalf("expected empty backfill when nothing precedes the trigger, got:\n%s", out)
	}
}

func TestBackfillFallsBackToIDOnUserLookupFailure(t *testing.T) {
	api := &fakeBackfillAPI{
		replies: []slack.Message{
			msg("1700000000.000100", "UUNKNOWN", "hello"),
			msg("1700000000.000300", "U1", "trigger"),
		},
		users: map[string]*slack.User{}, // every lookup fails
	}
	b := NewThreadBackfiller(api, "UBOT", nil)

	out, err := b.Backfill(context.Background(), "C1", "1700000000.000100", "1700000000.000300")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "@UUNKNOWN: hello") {
		t.Fatalf("expected fallback to raw id when lookup fails, got:\n%s", out)
	}
}

func TestBackfillCachesUserLookups(t *testing.T) {
	calls := 0
	api := &countingUserAPI{onUser: func() { calls++ }}
	b := NewThreadBackfiller(api, "", nil)
	_ = b.resolveName(context.Background(), "U1")
	_ = b.resolveName(context.Background(), "U1")
	if calls != 1 {
		t.Fatalf("expected the second lookup to hit the cache, got %d calls", calls)
	}
}

func TestBackfillPropagatesRepliesError(t *testing.T) {
	api := &fakeBackfillAPI{repErr: errors.New("rate limited")}
	b := NewThreadBackfiller(api, "UBOT", nil)
	if _, err := b.Backfill(context.Background(), "C1", "1700000000.000100", ""); err == nil {
		t.Fatal("expected the replies error to propagate so the caller can degrade")
	}
}

type countingUserAPI struct {
	onUser func()
}

func (c *countingUserAPI) GetConversationRepliesContext(_ context.Context, _ *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	return nil, false, "", nil
}

func (c *countingUserAPI) GetUserInfoContext(_ context.Context, _ string) (*slack.User, error) {
	c.onUser()
	return userWithDisplayName("cached"), nil
}
