package gateway

import (
	"context"

	"github.com/slack-go/slack"
)

// canvasInfoAPI is the slice of the Slack client slackCanvasInfo needs.
// *slack.Client satisfies it; tests inject a fake.
type canvasInfoAPI interface {
	GetConversationInfoContext(ctx context.Context, input *slack.GetConversationInfoInput) (*slack.Channel, error)
}

// slackCanvasInfo resolves a channel's canvas file id via conversations.info —
// the one extra hop needed to hand a canvas comment turn the id of the document
// it is attached to (spec 021 §9.3). A channel with no canvas resolves to "".
type slackCanvasInfo struct {
	api canvasInfoAPI
}

func (s slackCanvasInfo) ChannelCanvasFileID(ctx context.Context, channelID string) (string, error) {
	ch, err := s.api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: channelID})
	if err != nil {
		return "", err
	}
	if ch == nil || ch.Properties == nil {
		return "", nil // channel has no canvas
	}
	return ch.Properties.Canvas.FileId, nil
}
