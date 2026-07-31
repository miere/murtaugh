package gateway

import (
	"context"
	"sort"
	"strings"

	"github.com/slack-go/slack"
)

// canvasInfoAPI is the slice of the Slack client slackCanvasInfo needs.
// *slack.Client satisfies it; tests inject a fake.
type canvasInfoAPI interface {
	GetConversationInfoContext(ctx context.Context, input *slack.GetConversationInfoInput) (*slack.Channel, error)
}

// canvasFileAPI is the files.info slice needed to find the channel a canvas
// document lives in. *slack.Client satisfies it; tests inject a fake.
type canvasFileAPI interface {
	GetFileInfoContext(ctx context.Context, fileID string, count, page int) (*slack.File, []slack.Comment, *slack.Paging, error)
}

// canvasParentResolver maps a canvas file id to the channel its document lives
// in. channelNameCache uses it to route a file-backed canvas conversation by
// that parent channel; nil disables the hop (canvas turns then fall through to
// exact-ID/default).
type canvasParentResolver interface {
	CanvasParentChannel(ctx context.Context, fileID string) (channelID, channelName string, err error)
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
	// A file-backed canvas conversation carries no `properties`; its canvas file
	// id is encoded in name_normalized as "FC:<fileId>:<title>" (observed live:
	// channel C0…, name_normalized "FC:F0…:My Canvas"). This is the case Slice B
	// missed (spec 021 §9.3). NOTE: this shape does NOT mean "standalone" — a
	// channel TAB canvas reports it too; see CanvasParentChannel.
	return canvasFileIDFromName(ch.NameNormalized), nil
}

// slackCanvasParent resolves the channel a canvas document lives in, via
// files.info on the canvas file id (spec 021 §9.3).
//
// This exists because a file-backed canvas conversation's name_normalized
// ("FC:<fileId>:<title>") was long read as "standalone canvas, no parent". That
// is wrong: a Canvas TAB on a real channel reports the very same shape, so the
// conversation cannot tell you the parent — but the FILE can. files.info returns
// `shares.{private,public}` keyed by channel id and carrying channel_name inline,
// plus the `groups`/`channels` id lists.
//
// Observed against the live workspace: file F0BLEABKSCD (conversation
// C0BLEABKSCD, name_normalized "FC:F0BLEABKSCD:…") returns
// shares.private["C0BLBB1LTUK"] with channel_name "nc-proj-spdd" and
// source "CHANNEL_TAB" — the channel whose `nc-*` rule routes to the coder
// agent, and which every canvas turn had been silently defaulting away from.
//
// Requires the files:read scope. A canvas genuinely shared nowhere resolves to
// ("", "", nil) and routing falls through to exact-ID/default as before.
type slackCanvasParent struct {
	api canvasFileAPI
}

func (s slackCanvasParent) CanvasParentChannel(ctx context.Context, fileID string) (string, string, error) {
	if s.api == nil || fileID == "" {
		return "", "", nil
	}
	file, _, _, err := s.api.GetFileInfoContext(ctx, fileID, 0, 0)
	if err != nil {
		return "", "", err
	}
	if file == nil {
		return "", "", nil
	}
	// shares carries channel_name inline, so a hit here needs no second lookup.
	// Private is consulted first: a canvas tab on a private channel reports under
	// shares.private and leaves `channels` empty (the observed case above).
	if id, name := firstSharedChannel(file.Shares.Private); id != "" {
		return id, name, nil
	}
	if id, name := firstSharedChannel(file.Shares.Public); id != "" {
		return id, name, nil
	}
	// Fall back to the bare id lists, which carry no name — the caller resolves
	// it with one ordinary conversations.info on the parent.
	if id := firstChannelID(file.Groups); id != "" {
		return id, "", nil
	}
	return firstChannelID(file.Channels), "", nil
}

// firstSharedChannel picks one channel from a shares map, preferring an entry
// that carries a channel_name. A canvas shared into several channels has no
// single right answer, so the choice is made DETERMINISTIC by sorting the ids
// rather than taking Go's randomised map order — the same canvas must route to
// the same agent on every turn, even if that channel is not the one an operator
// would have picked. Such a canvas is better pinned with an exact-ID rule.
func firstSharedChannel(shares map[string][]slack.ShareFileInfo) (string, string) {
	ids := make([]string, 0, len(shares))
	for id := range shares {
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return "", ""
	}
	sort.Strings(ids)
	id := ids[0]
	for _, share := range shares[id] {
		if share.ChannelName != "" {
			return id, share.ChannelName
		}
	}
	return id, ""
}

// firstChannelID returns the lowest id, sorting a COPY so the caller's slice is
// never reordered. Same determinism rationale as firstSharedChannel.
func firstChannelID(ids []string) string {
	present := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			present = append(present, id)
		}
	}
	if len(present) == 0 {
		return ""
	}
	sort.Strings(present)
	return present[0]
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
