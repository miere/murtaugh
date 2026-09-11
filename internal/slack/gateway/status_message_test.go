package gateway

import (
	"context"
	"testing"

	"github.com/slack-go/slack"
)

type fakeStatusMessenger struct {
	posts   int
	updates int

	postChannel   string
	postThreadTS  string
	postOptions   []slack.MsgOption
	updateTS      string
	updateOptions []slack.MsgOption
}

func (f *fakeStatusMessenger) PostMessageContext(_ context.Context, channelID string, options ...slack.MsgOption) (string, string, error) {
	f.posts++
	f.postChannel = channelID
	f.postOptions = options
	f.postThreadTS = optionValue(options, "thread_ts")
	return channelID, "status-ts", nil
}

func (f *fakeStatusMessenger) UpdateMessageContext(_ context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error) {
	f.updates++
	f.updateTS = timestamp
	f.updateOptions = options
	return channelID, timestamp, "", nil
}

func optionValue(options []slack.MsgOption, key string) string {
	_, values, err := slack.UnsafeApplyMsgOptions("xoxb-test", "C1", "https://slack.com/api", options...)
	if err != nil {
		return ""
	}
	return values.Get(key)
}

func TestStatusContextBlockShape(t *testing.T) {
	block, ok := statusContextBlock("Reading file…").(*slack.ContextBlock)
	if !ok {
		t.Fatalf("expected a *slack.ContextBlock")
	}
	if len(block.ContextElements.Elements) != 1 {
		t.Fatalf("expected one element, got %d", len(block.ContextElements.Elements))
	}
	text, ok := block.ContextElements.Elements[0].(*slack.TextBlockObject)
	if !ok {
		t.Fatalf("expected a plain_text element, got %T", block.ContextElements.Elements[0])
	}
	if text.Type != slack.PlainTextType || text.Text != "Reading file…" {
		t.Fatalf("unexpected text object: %+v", text)
	}
	if text.Emoji == nil || !*text.Emoji {
		t.Fatalf("expected emoji:true on the plain_text element, got %v", text.Emoji)
	}
}
