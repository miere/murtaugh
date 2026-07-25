package gateway

import (
	"context"
	"strings"

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
	if ch == nil {
		return "", nil
	}
	// A channel canvas (a Canvas tab on a real channel) carries its file id in
	// properties.canvas.
	if ch.Properties != nil && ch.Properties.Canvas.FileId != "" {
		return ch.Properties.Canvas.FileId, nil
	}
	// A standalone canvas (created via + → Canvas) is a file-backed conversation
	// with no `properties`; its canvas file id is encoded in name_normalized as
	// "FC:<fileId>:<title>" (verified against a live standalone canvas: channel
	// C0…, name_normalized "FC:F0…:My Canvas"). This is the case Slice B missed
	// (spec 021 §9.3).
	return canvasFileIDFromName(ch.NameNormalized), nil
}

// canvasFileIDFromName extracts the canvas file id from a file-backed canvas
// conversation's name_normalized ("FC:<fileId>:<title>"), or "" when it is not
// that shape.
func canvasFileIDFromName(nameNormalized string) string {
	const prefix = "FC:"
	if !strings.HasPrefix(nameNormalized, prefix) {
		return ""
	}
	rest := nameNormalized[len(prefix):]
	if i := strings.IndexByte(rest, ':'); i > 0 {
		return rest[:i]
	}
	return ""
}
