package gateway

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/slack-go/slack"
)

// requestTranslator turns the Slack side of a turn — the triggering message, the
// files uploaded with it, the routing decision, and the thread it lives in — into
// the request that is handed to whoever runs the agent. It is the first of the
// two named translations the gateway/runtime split needs (#188): Slack event →
// protocol.
//
// # Why it produces agent types rather than wire types
//
// The request direction's wire shape is `agent.SessionMetadata` (already JSON
// tagged), `agent.ConversationKey` and `agent.PromptRequest` — see #170 Change C,
// "most of this direction is already wire-shaped". The hop from those values to
// frames on a socket is the transport's job and does not exist yet. So this type
// stops exactly where the Slack vocabulary stops: it consumes `ChatRequest` and
// `ChatRoute` and produces the agent layer's own request values, which the local
// session manager and a remote node both take. Making it emit a bespoke wire
// struct here would put a second, untested encoding beside `internal/agentwire`
// and force the in-process path to decode its own output.
//
// # The layer rule, from this side
//
// This half never touches a renderer and never sees an event; the other half
// (eventTranslator) never sees a Slack message, a file upload or a routing
// decision. Neither reaches past the other, which is the property #188 asks for.
//
// # What it deliberately does not do
//
// It does not call `Prompt`, does not register the background-events target,
// does not set the assistant status and does not decide what happens when it
// fails. It resolves values; the caller acts on them. The two failures it CAN
// report are the two the turn cannot proceed without — no prompt at all, and no
// Slack timestamp to post against — and both are sentinels so a caller can tell
// them apart without matching prose.
//
// # Impure inputs
//
// Three, each behind the narrow interface (or function) the handler already uses:
// the thread backfiller, the canvas resolver, and the fold-uploaded-files step.
// All three are optional; a nil one degrades that feature rather than failing the
// turn, exactly as the handler does today.
//
// Nothing calls this yet. It is Stage 1 of #170 — new code, no callers, the
// serving path untouched. The logic it mirrors is the opening ~50 lines of
// ChatHandler.Handle; until the wiring stage lands, the two are duplicates and
// the tests below are what keeps them from drifting.
type requestTranslator struct {
	backfiller threadBackfiller
	canvasInfo canvasInfoResolver
	// foldFiles renders the message's plain-text uploads into a block appended to
	// the prompt. It is a function rather than an interface because the only
	// implementation is a method on ChatHandler that carries the file fetcher and
	// its budgets (renderAttachments); the translator cares only that some text
	// comes back. nil means no attachment handling.
	foldFiles func(context.Context, []slack.File) string
	logger    *slog.Logger
}

// errEmptyPrompt reports a turn with neither message text nor a readable
// upload — there is nothing to ask the agent.
var errEmptyPrompt = errors.New("chat prompt is empty")

// errNoSourceTimestamp reports a turn with no Slack timestamp to post against.
// Checked separately from the reply thread, which is empty BY DESIGN in
// channel-reply mode (post at the channel root): emptiness there is a strategy,
// not a fault, so the guard is on the triggering message instead.
var errNoSourceTimestamp = errors.New("Slack streaming requires a source message timestamp")

// turnRequest is everything resolved before the first event arrives: what the
// agent is asked, which conversation it belongs to, and where the answer goes.
type turnRequest struct {
	// Key is the conversation the agent session is bound to.
	Key agent.ConversationKey
	// Metadata is the session's context — surface, channel, thread, user.
	Metadata agent.SessionMetadata
	// Prompt is what the agent is asked, including any backfilled history.
	Prompt agent.PromptRequest
	// UserText is the human's own message, trimmed, WITHOUT the folded upload
	// block. Kept apart from Prompt.Text because the journal records what the
	// person wrote, not what the file contained.
	UserText string
	// ReplyThreadTS is where the reply is posted. Empty is meaningful: it is
	// channel-reply mode, posting at the channel root.
	ReplyThreadTS string
}

// sessionLookup reports whether a conversation already has a live agent session.
// ChatSessionManager satisfies it; the translator needs only this one method,
// because the answer decides exactly one thing — whether the thread still needs
// backfilling, or the warm session already holds it.
type sessionLookup interface {
	Lookup(agent.ConversationKey) (string, bool)
}

// newRequestTranslator builds a translator. Every dependency is optional: a nil
// backfiller starts sessions cold, a nil canvas resolver records the canvas
// surface without an id, and a nil foldFiles ignores uploads.
func newRequestTranslator(backfiller threadBackfiller, canvasInfo canvasInfoResolver, foldFiles func(context.Context, []slack.File) string, logger *slog.Logger) *requestTranslator {
	if logger == nil {
		logger = slog.Default()
	}
	return &requestTranslator{backfiller: backfiller, canvasInfo: canvasInfo, foldFiles: foldFiles, logger: logger}
}

// Translate resolves one Slack turn into the request the agent is given.
// sessions may be nil; it is consulted only to skip backfilling a warm session.
//
// The two guards run before any Slack read, so a turn that cannot proceed costs
// no API calls. ChatHandler.Handle runs them the other way round and further in:
// the empty prompt first, the timestamp last, after the metadata and the
// backfill. For a turn that passes both, the resolved values are identical.
//
// For a turn that fails, two things differ, and the wiring stage has to move the
// callers rather than assume the swap is invisible:
//
//   - A turn that trips BOTH guards is reported here as errNoSourceTimestamp and
//     by Handle as its empty-prompt error.
//   - The errors are prose-alike, not value-alike: Handle builds fresh fmt.Errorf
//     values, so errors.Is(handleErr, errEmptyPrompt) is false. Matching on these
//     sentinels only works once Handle produces them.
func (t *requestTranslator) Translate(ctx context.Context, req ChatRequest, route ChatRoute, sessions sessionLookup) (turnRequest, error) {
	if req.ThreadTS == "" && req.MessageTS == "" {
		return turnRequest{}, errNoSourceTimestamp
	}
	text := strings.TrimSpace(req.Text)
	// Fold any plain-text attachments into the message the agent sees. A
	// caption-less upload (no text, only files) is still a valid prompt.
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
	// A canvas comment turn: mark the surface, resolve the canvas id, and tell the
	// agent (in-context) which document it is looking at so it can read/edit it
	// (spec 021 §9.3). Discovery runs once, on the cold session that produced the
	// backfill; warm turns already carry this.
	if canvas != nil {
		out.Metadata.Surface = "canvas"
		out.Metadata.CanvasID = t.canvasID(ctx, req.ChannelID)
		history = prependCanvasNote(history, out.Metadata.CanvasID)
		t.logger.Info("canvas comment turn discovered", "channel", req.ChannelID, "canvas_id", out.Metadata.CanvasID)
	}
	out.Prompt = agent.PromptRequest{Text: promptText, History: history}
	return out, nil
}

// backfill renders the existing Slack thread into a transcript to seed a
// brand-new session, returning "" when no backfill is needed or possible.
//
// Only a threaded conversation whose session is not already live is backfilled:
// a warm session already holds the history, and a top-level message has no prior
// thread to read. The triggering message is excluded so it is not duplicated
// ahead of the user's own prompt text. A fetch failure degrades to "" — the agent
// proceeds without backstory rather than the turn failing.
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

// canvasID resolves the channel's canvas file id, or "" when there is no
// resolver wired or the lookup fails. The surface is recorded either way: knowing
// the turn came from a canvas is worth more than the id, and a canvas turn must
// not fail because conversations.info did.
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
