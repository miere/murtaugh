package gateway

import (
	"context"

	"github.com/slack-go/slack"
)

type statusMessenger interface {
	PostMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error)
	UpdateMessageContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, string, error)
}

func statusMsgOptions(text string) []slack.MsgOption {
	return []slack.MsgOption{
		slack.MsgOptionText(text, false),
		slack.MsgOptionBlocks(statusContextBlock(text)),
	}
}

func statusContextBlock(text string) slack.Block {
	return slack.NewContextBlock("", slack.NewTextBlockObject(slack.PlainTextType, text, true, false))
}
