package gateway

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/slack-go/slack"
)

type requestTranslator struct {
	backfiller threadBackfiller
	canvasInfo canvasInfoResolver
	foldFiles  func(context.Context, []slack.File) string
	logger     *slog.Logger
}

var errEmptyPrompt = errors.New("chat prompt is empty")

var errNoSourceTimestamp = errors.New("Slack streaming requires a source message timestamp")

type turnRequest struct {
	Key           agent.ConversationKey
	Metadata      agent.SessionMetadata
	Prompt        agent.PromptRequest
	UserText      string
	ReplyThreadTS string
}

type sessionLookup interface {
	Lookup(agent.ConversationKey) (string, bool)
}

func newRequestTranslator(backfiller threadBackfiller, canvasInfo canvasInfoResolver, foldFiles func(context.Context, []slack.File) string, logger *slog.Logger) *requestTranslator {
	if logger == nil {
		logger = slog.Default()
	}
	return &requestTranslator{backfiller: backfiller, canvasInfo: canvasInfo, foldFiles: foldFiles, logger: logger}
}

func (t *requestTranslator) Translate(ctx context.Context, req ChatRequest, route ChatRoute, sessions sessionLookup) (turnRequest, error) {
	if req.ThreadTS == "" && req.MessageTS == "" {
		return turnRequest{}, errNoSourceTimestamp
	}
	text := strings.TrimSpace(req.Text)
	var uploads string
	if t.foldFiles != nil {
		uploads = t.foldFiles(ctx, req.Files)
	}
	if text == "" && uploads == "" {
		return turnRequest{}, errEmptyPrompt
	}
	promptText := text
	switch {
	case uploads == "":
	case promptText == "":
		promptText = uploads
	default:
		promptText += "\n\n" + uploads
	}

	key := conversationKey(req, route.ReplyOnThread)
	out := turnRequest{
		Key: key,
		Metadata: agent.SessionMetadata{
			TeamID: req.TeamID, ChannelID: req.ChannelID, ThreadTS: key.ThreadTS,
			UserID: req.UserID, Source: req.Source,
		},
		UserText:      text,
		ReplyThreadTS: replyThreadTS(req, route.ReplyOnThread),
	}

	history, canvas := t.backfill(ctx, req, key, sessions)
	if canvas != nil {
		out.Metadata.Surface = "canvas"
		out.Metadata.CanvasID = t.canvasID(ctx, req.ChannelID)
		history = prependCanvasNote(history, out.Metadata.CanvasID)
		t.logger.Info("canvas comment turn discovered", "channel", req.ChannelID, "canvas_id", out.Metadata.CanvasID)
	}
	out.Prompt = agent.PromptRequest{Text: promptText, History: history}
	return out, nil
}

func (t *requestTranslator) backfill(ctx context.Context, req ChatRequest, key agent.ConversationKey, sessions sessionLookup) (string, *CanvasContext) {
	if t.backfiller == nil || req.ThreadTS == "" {
		return "", nil
	}
	if sessions != nil {
		if _, live := sessions.Lookup(key); live {
			return "", nil
		}
	}
	history, canvas, err := t.backfiller.BackfillWithSurface(ctx, req.ChannelID, req.ThreadTS, req.MessageTS)
	if err != nil {
		t.logger.Warn("thread backfill failed; proceeding without history", "channel", req.ChannelID, "thread", req.ThreadTS, "error", err)
		return "", nil
	}
	if history != "" {
		t.logger.Info("seeding new agent session with thread history", "channel", req.ChannelID, "thread", req.ThreadTS)
	}
	return history, canvas
}

func (t *requestTranslator) canvasID(ctx context.Context, channelID string) string {
	if t.canvasInfo == nil {
		return ""
	}
	fileID, err := t.canvasInfo.ChannelCanvasFileID(ctx, channelID)
	if err != nil {
		t.logger.Warn("failed to resolve canvas id; proceeding without it", "channel", channelID, "error", err)
		return ""
	}
	return fileID
}
