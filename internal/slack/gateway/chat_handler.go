package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/slack/alertcard"
	"github.com/slack-go/slack"
)

// defaultStatusRefreshInterval controls how often Handle re-asserts the
// "is thinking..." assistant status. Slack auto-clears the status as soon as
// the first stream chunk lands, so without periodic refresh the indicator
// disappears the moment streaming starts while the response is still being
// assembled. Two seconds is a compromise between responsiveness and API churn.
const defaultStatusRefreshInterval = 2 * time.Second

// defaultIdleTimeout bounds a chat turn by inactivity rather than total
// wall-clock time. The timer resets on every event the agent emits (text or a
// task update), so a long turn that keeps making progress never trips it; only
// an agent that goes completely silent for this long is treated as stalled.
const defaultIdleTimeout = 10 * time.Minute

// idleTimeoutAside is posted as its own discrete context-block message when a
// turn is abandoned for inactivity. A stall is almost always the agent leaving
// its own turn open (not a Murtaugh fault), so the copy is a light nudge rather
// than an alarm appended to the reply — and Murtaugh gets his line in. It lands
// below the settled reply as a low-key aside, inviting the user to continue.
const idleTimeoutAside = "Agent's taking a nap like it's earned one. Feel free to nudge it. I'm too old for this!"

// ChatSessionManager is the narrow surface the gateway uses to talk to
// the ACP layer. Prompt drives the streaming response, while Lookup and
// Cancel back the interrupt-handling path (Gateway.startChat's previous
// in-flight chat cancellation and the /stop slash command). Lookup is
// side-effect-free: callers must treat (_, false) as "no live session,
// skip the ACP cancel call" rather than implicitly opening one.
type ChatSessionManager interface {
	Prompt(context.Context, agent.ConversationKey, agent.SessionMetadata, agent.PromptRequest) (<-chan agent.Event, error)
	Lookup(agent.ConversationKey) (string, bool)
	Cancel(context.Context, string) error
}

type ChatSessionWarmer interface {
	Warm(context.Context) error
}

type ChatHandler struct {
	api                   StreamAPI
	sessions              map[string]ChatSessionManager
	resolver              func(ChatRequest) ChatRoute
	interval              time.Duration
	minChars              int
	logger                *slog.Logger
	statusRefreshInterval time.Duration
	idleTimeout           time.Duration
	// sessionLog records each turn to the journal's acp_session stream. nil
	// (the default) records nothing; the gateway wires it only when that stream
	// is enabled.
	sessionLog *sessionLogger
	// templateDir is where the buffered reply's Block Kit template is looked up
	// before the embedded assets tree, so an operator can restyle it without a
	// rebuild. Empty means the working directory (the embedded default wins).
	templateDir string
	// userNames resolves a Slack user id to a display name when the buffered
	// transport rewrites mentions out of the reply prose. A nil cache resolves
	// nothing and the raw id is shown — cosmetic only; the mention still fires.
	userNames       *userNameCache
	statusMessenger statusMessenger
	// backfiller renders an existing Slack thread into a transcript when a brand-
	// new ACP session is opened for it, so the agent starts with the prior
	// conversation as context. nil disables backfill (the prompt is the single
	// triggering message only).
	backfiller threadBackfiller
	// canvasInfo resolves a channel's canvas file id when a turn originates from a
	// canvas comment thread, so the id reaches the agent's context and session
	// metadata (spec 021 §9.3). nil records the canvas surface without an id.
	canvasInfo canvasInfoResolver
	// fileFetcher downloads plain-text attachments so their contents can be
	// folded into the prompt. nil disables attachment handling (tests that do
	// not wire Slack); the gateway always supplies it in production.
	fileFetcher fileFetcher
	// uploader delivers agent-produced attachments (EventAttachment) into the
	// turn's thread. nil disables outbound attachments (tests that do not wire
	// Slack); the gateway always supplies it in production.
	uploader attachmentUploader
	// permissionAskers resolves an ACP agent's EventPermission with a human (Slack
	// approval buttons), keyed by the agent the turn is running as. It is consulted
	// on the turn's own event loop so the approval card is ordered with the reply —
	// the chat handler settles any open reply text first, exactly as the native
	// loop's inline approval is ordered. A missing entry denies every ACP
	// permission request (no human wired), which keeps a headless turn from
	// hanging.
	//
	// It is keyed per agent rather than one shared gate because the gate carries
	// that agent's approval settings — today, whether a settled card is kept or
	// swept.
	permissionAskers map[string]agent.PermissionAsker
	display          displayer
	// backgroundEventsRouter renders a claude_code background completion (a subagent
	// finishing after its turn ended) into the conversation's thread. Handle
	// registers each turn's thread with it so the router knows where to post. nil
	// disables background rendering (tests, non-claude_code deploys).
	backgroundEventsRouter *backgroundEventsRouter
	// alertCards renders the turn-ending alerts (a failure, an empty reply) as
	// collapsed cards, and alertAPI posts them. Both nil (tests that do not wire
	// Slack) makes the renderer paint those alerts as text on the reply surface
	// instead — the same fallback a failed post takes.
	alertCards *alertcard.Renderer
	alertAPI   alertMessagePoster
	// offlineOwners holds the machine-offline card's mention backoff across turns.
	// A gateway restart forgets it, which costs the owner one more mention.
	offlineOwners *ownerNotifyWindow
	// credRepair asks the admin to re-authenticate when a claude_code turn fails
	// because its credential was rejected. It covers the case auth.request
	// structurally cannot: that tool is called by an agent from inside a turn, but
	// a bad credential stops the agent running at all, so nobody is left to call
	// it. nil leaves such failures reported as ordinary errors.
	credRepair *credentialRepair
}

// threadBackfiller renders a Slack thread into a transcript block for a cold
// session's first prompt, and reports whether that thread is a canvas comment
// (non-nil CanvasContext). *ThreadBackfiller satisfies it.
type threadBackfiller interface {
	BackfillWithSurface(ctx context.Context, channelID, threadTS, excludeTS string) (string, *CanvasContext, error)
}

// canvasInfoResolver resolves a channel's canvas file id (F…) for a canvas
// comment turn, so the agent's context carries the id it needs to read/edit the
// document. A gateway wraps conversations.info; nil leaves canvas turns without an
// id (Surface is still recorded).
type canvasInfoResolver interface {
	ChannelCanvasFileID(ctx context.Context, channelID string) (string, error)
}

type ChatRequest struct {
	TeamID    string
	ChannelID string
	UserID    string
	ThreadTS  string
	MessageTS string
	Text      string
	DM        bool
	Source    string
	// AgentOverride pins the agent for this turn, bypassing channel routing.
	// Empty means route by channel as usual. Set by a delegate-to-agent trigger
	// whose config names an explicit agent.
	AgentOverride string
	// Files carries any attachments on the triggering Slack message. Plain-text
	// files are fetched and folded into the prompt so the agent can read them.
	Files []slack.File
}

// ChatRoute is the routing decision for a chat request: which agent answers and
// whether the reply is threaded. ReplyOnThread=false makes the bot post directly
// in the channel; see replyThreadTS and conversationKey for how it shapes both
// the reply location and the session binding.
type ChatRoute struct {
	Agent         string
	ReplyOnThread bool
	// ChannelName is left out of the conversation key, so correcting it later in
	// dispatchTurn cannot change which session a turn is bound to.
	ChannelName string
}

func NewChatHandler(api StreamAPI, sessions map[string]ChatSessionManager, resolver func(ChatRequest) ChatRoute, interval time.Duration, minChars int, logger *slog.Logger) *ChatHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &ChatHandler{
		api: api, sessions: sessions, resolver: resolver, interval: interval, minChars: minChars,
		logger: logger, statusRefreshInterval: defaultStatusRefreshInterval,
		offlineOwners: newOwnerNotifyWindow(ownerNotifyBackoff, time.Now),
	}
}

// WithCredentialRepair attaches the path that asks the admin to re-authenticate
// a rejected Claude Code credential. nil (the default) leaves credential
// failures reported as ordinary turn errors. Returns the handler for chaining.
func (h *ChatHandler) WithCredentialRepair(r *credentialRepair) *ChatHandler {
	h.credRepair = r
	return h
}

// errCredentialBlocked is what the user sees when their turn failed only because
// the agent's credential was rejected. It replaces the raw error deliberately:
// the CLI's own text tells them to run /login, which they cannot do and which is
// not their job — the admin owns the credential.
var errCredentialBlocked = errors.New(
	"Claude Code's credentials were rejected, so this turn could not run. " +
		"I've asked your admin to re-authenticate — try again once they have.")

var errOwnerSigningIn = errors.New(
	"Claude Code's credentials were rejected on the machine this agent runs on, so this turn could not run. " +
		"Its owner has been sent a sign-in by direct message — try again once they have finished it.")

// failedOnCredential reports the turn as blocked-on-credentials and starts a
// repair, or returns false when this is an ordinary failure the caller should
// report as-is.
func (h *ChatHandler) failedOnCredential(agentName string, err error) bool {
	return h.credRepair.Handles(agentName, err) && h.credRepair.Request(agentName).started()
}

// WithIdleTimeout sets how long a turn may go without any agent activity before
// it is treated as stalled. Non-positive values are ignored so the default
// stands. Returns the handler for chaining.
func (h *ChatHandler) WithIdleTimeout(d time.Duration) *ChatHandler {
	if d > 0 {
		h.idleTimeout = d
	}
	return h
}

func (h *ChatHandler) effectiveIdleTimeout() time.Duration {
	if h.idleTimeout > 0 {
		return h.idleTimeout
	}
	return defaultIdleTimeout
}

func (h *ChatHandler) postIdleAside(ctx context.Context, channelID, threadTS string) {
	if h.statusMessenger == nil {
		return
	}
	options := statusMsgOptions(idleTimeoutAside)
	if threadTS != "" {
		options = append(options, slack.MsgOptionTS(threadTS))
	}
	if _, _, err := h.statusMessenger.PostMessageContext(ctx, channelID, options...); err != nil {
		h.logger.Warn("failed to post idle-timeout aside", "error", err)
	}
}

// WithSessionLogger attaches the acp_session turn recorder and returns the
// handler for chaining. nil leaves session logging off.
func (h *ChatHandler) WithSessionLogger(sl *sessionLogger) *ChatHandler {
	h.sessionLog = sl
	return h
}

// A canvas cannot host a stream, and the idle-timeout aside is not part of one.
func (h *ChatHandler) WithStatusMessenger(m statusMessenger) *ChatHandler {
	h.statusMessenger = m
	return h
}

// WithBackfiller wires the thread backfiller that seeds a cold ACP session with
// the existing Slack thread. Returns the handler for chaining. nil (the default)
// disables backfill. A nil *ThreadBackfiller is also tolerated so callers can
// wire it unconditionally and let an unbuilt backfiller stay disabled.
func (h *ChatHandler) WithBackfiller(b *ThreadBackfiller) *ChatHandler {
	if b == nil {
		return h
	}
	h.backfiller = b
	return h
}

// WithCanvasInfo wires the resolver that maps a channel to its canvas file id for
// canvas comment turns. Returns the handler for chaining. nil (the default)
// records the canvas surface without an id.
func (h *ChatHandler) WithCanvasInfo(r canvasInfoResolver) *ChatHandler {
	h.canvasInfo = r
	return h
}

// WithFileFetcher wires the downloader used to fold plain-text attachments into
// the prompt. Returns the handler for chaining. nil (the default) disables
// attachment handling.
func (h *ChatHandler) WithFileFetcher(f fileFetcher) *ChatHandler {
	if f == nil {
		return h
	}
	h.fileFetcher = f
	return h
}

// WithUploader wires the surface used to deliver agent-produced attachments into
// the turn's thread. Returns the handler for chaining. nil (the default) disables
// outbound attachments.
func (h *ChatHandler) WithUploader(u attachmentUploader) *ChatHandler {
	if u == nil {
		return h
	}
	h.uploader = u
	return h
}

// WithReplyBlocks wires what the buffered reply transport needs to render its
// Block Kit document: the operator template directory (empty means the embedded
// default) and the Slack surface used to turn a tagged user id into a name.
// Returns the handler for chaining. A nil api leaves names unresolved, which is
// cosmetic — mentions still notify.
func (h *ChatHandler) WithReplyBlocks(templateDir string, api userInfoAPI) *ChatHandler {
	h.templateDir = templateDir
	if api != nil {
		h.userNames = newUserNameCache(api, h.logger)
	}
	return h
}

// WithPermissionAskers wires the per-agent gates that resolve an ACP agent's
// permission requests (EventPermission) with a human. Returns the handler for
// chaining. nil/empty (the default) leaves the handler denying every ACP
// permission request.
func (h *ChatHandler) WithPermissionAskers(askers map[string]agent.PermissionAsker) *ChatHandler {
	if len(askers) == 0 {
		return h
	}
	h.permissionAskers = askers
	return h
}

type displayer interface {
	Question(ctx context.Context, loc agent.TurnLocation, req agent.QuestionRequest) (agent.DisplayAnswer, error)
	Plan(ctx context.Context, loc agent.TurnLocation, req agent.PlanRequest) (agent.DisplayAnswer, error)
	SignIn(ctx context.Context, loc agent.TurnLocation, prompt *agent.SignInPrompt, settled <-chan agent.SignInSettled)
}

func (h *ChatHandler) WithDisplay(d displayer) *ChatHandler {
	h.display = d
	return h
}

// WithBackgroundEventsRouter wires the router that renders claude_code background
// completions into their thread. nil leaves background rendering off. Returns the
// handler for chaining.
func (h *ChatHandler) WithBackgroundEventsRouter(s *backgroundEventsRouter) *ChatHandler {
	h.backgroundEventsRouter = s
	return h
}

// WithAlerts wires the alert-card renderer and the client that posts it. dir is
// looked up before the embedded assets tree, so an operator can restyle alerts
// without a rebuild. A nil api leaves alerts as text on the reply surface.
// Returns the handler for chaining.
func (h *ChatHandler) WithAlerts(dir string, api alertMessagePoster) *ChatHandler {
	h.alertCards = alertcard.NewRenderer(dir, assets.FS)
	h.alertAPI = api
	return h
}

func (h *ChatHandler) newChatRenderer(channelID, threadTS string, opts StreamWriterOptions) chatRenderer {
	newBlock := func() toolBlock {
		return newDefaultCardBlock(h.api, h.statusMessenger, channelID, threadTS, opts, h.logger)
	}
	return newSectionRenderer(
		// The reply-text transport negotiates streaming vs buffered posting: a
		// canvas surface (which cannot stream) downgrades to chat.postMessage
		// instead of erroring the whole turn (spec 021, issue #87).
		func() SlackSink { return newDefaultSlackSink(h.api, h.statusMessenger, channelID, opts, h.logger) },
		newBlock,
		h.uploader,
		newAlertPoster(h.alertAPI, h.alertCards, channelID, threadTS),
		newOfflineOwnerRef(h.offlineOwners, h.userNames),
		channelID,
		threadTS,
		h.logger,
	)
}

// resetIdleTimer restarts t for another idle window, draining an already-fired
// timer first so the next select does not observe a stale tick.
func resetIdleTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// discardSession drops the conversation→session binding when a session may be
// wedged — an idle timeout or a tool that blew past its ceiling — and the agent
// cannot be told to abandon its in-flight work (no session/cancel). The next turn
// then opens a fresh session; the shared agent process is left running.
func discardSession(sessions ChatSessionManager, key agent.ConversationKey) {
	if d, ok := sessions.(interface {
		Discard(agent.ConversationKey)
	}); ok {
		d.Discard(key)
	}
}

func (h *ChatHandler) Warm(ctx context.Context) error {
	for name, manager := range h.sessions {
		warmer, ok := manager.(ChatSessionWarmer)
		if !ok {
			continue
		}
		if err := warmer.Warm(ctx); err != nil {
			h.logger.Warn("failed to warm agent", "agent", name, "error", err)
		}
	}
	return nil
}

// backfillHistory renders the existing Slack thread into a transcript to seed a
// brand-new ACP session, returning "" when no backfill is needed or possible.
// It backfills only a threaded conversation (ThreadTS set) whose session is not
// already live: a warm session already holds the history, and a top-level
// message has no prior thread to read. The triggering message (MessageTS) is
// excluded so it is not duplicated ahead of the user's own prompt text. A fetch
// failure is logged and degraded to "" — the agent proceeds without backstory
// rather than the turn failing.
func (h *ChatHandler) backfillHistory(ctx context.Context, req ChatRequest, sessions ChatSessionManager, key agent.ConversationKey) (string, *CanvasContext) {
	if h.backfiller == nil || req.ThreadTS == "" {
		return "", nil
	}
	if _, live := sessions.Lookup(key); live {
		return "", nil
	}
	history, canvas, err := h.backfiller.BackfillWithSurface(ctx, req.ChannelID, req.ThreadTS, req.MessageTS)
	if err != nil {
		h.logger.Warn("thread backfill failed; proceeding without history", "channel", req.ChannelID, "thread", req.ThreadTS, "error", err)
		return "", nil
	}
	if history != "" {
		h.logger.Info("seeding new agent session with thread history", "channel", req.ChannelID, "thread", req.ThreadTS)
	}
	return history, canvas
}

// prependCanvasNote frames the canvas surface for the agent — it is looking at a
// Slack canvas document, and (when resolved) the canvas id it can act on — ahead
// of the thread transcript, so the model reads the framing before the section
// content (spec 021 §9.3).
func prependCanvasNote(history, canvasID string) string {
	note := "<canvas-context>\nYou were mentioned inside a Slack canvas — a shared, editable document."
	if canvasID != "" {
		note += fmt.Sprintf(" Its canvas id is %s; use it with the canvas tools to read or edit the document.", canvasID)
	}
	note += "\nThe transcript below includes the canvas section you were tagged in."
	note += "\n</canvas-context>"
	if history == "" {
		return note
	}
	return note + "\n\n" + history
}

// Handle runs one chat turn. route is the already-resolved routing decision
// (agent + reply strategy) computed and corrected upstream in Gateway.dispatchTurn
// — Handle does NOT re-resolve, so its conversation key is provably the same one
// the coalescer/interrupt registry used.
func (h *ChatHandler) Handle(ctx context.Context, req ChatRequest, route ChatRoute) (retErr error) {
	startedAt := time.Now()
	if h == nil || len(h.sessions) == 0 {
		return fmt.Errorf("chat is not enabled")
	}

	agentName := route.Agent
	sessions, ok := h.sessions[agentName]
	if !ok {
		return fmt.Errorf("no agent configured for %q (resolved from request)", agentName)
	}

	prompt := strings.TrimSpace(req.Text)
	// Fold any plain-text attachments into the message the agent sees. A
	// caption-less upload (no text, only files) is still a valid prompt.
	attachments := h.renderAttachments(ctx, req.Files)
	if prompt == "" && attachments == "" {
		return fmt.Errorf("chat prompt is empty")
	}
	promptForAgent := prompt
	if attachments != "" {
		if promptForAgent == "" {
			promptForAgent = attachments
		} else {
			promptForAgent += "\n\n" + attachments
		}
	}
	key := conversationKey(req, route.ReplyOnThread)
	metadata := agent.SessionMetadata{TeamID: req.TeamID, ChannelID: req.ChannelID, ChannelName: route.ChannelName, ThreadTS: key.ThreadTS, UserID: req.UserID, Source: req.Source}
	history, canvas := h.backfillHistory(ctx, req, sessions, key)
	// A canvas comment turn: mark the surface, resolve the canvas id, and tell the
	// agent (in-context) which document it is looking at so it can read/edit it
	// (spec 021 §9.3). Discovery runs once, on the cold session that produced the
	// backfill; warm turns already carry this.
	if canvas != nil {
		metadata.Surface = "canvas"
		if h.canvasInfo != nil {
			if fileID, err := h.canvasInfo.ChannelCanvasFileID(ctx, req.ChannelID); err != nil {
				h.logger.Warn("failed to resolve canvas id; proceeding without it", "channel", req.ChannelID, "error", err)
			} else if fileID != "" {
				metadata.CanvasID = fileID
			}
		}
		history = prependCanvasNote(history, metadata.CanvasID)
		h.logger.Info("canvas comment turn discovered", "channel", req.ChannelID, "canvas_id", metadata.CanvasID)
	}
	streamThreadTS := replyThreadTS(req, route.ReplyOnThread)
	// streamThreadTS is empty by design in channel-reply mode (post at the channel
	// root). Posting still needs a triggering timestamp, so guard on that instead.
	if req.ThreadTS == "" && req.MessageTS == "" {
		return fmt.Errorf("Slack streaming requires a source message timestamp")
	}

	// Session-log accumulation. Declared before the defers below so the deferred
	// recorder (registered first → runs last, after the interrupt handler has
	// settled retErr) reads the final outcome, transcript, and session id.
	var (
		respBuf    strings.Builder
		chunkSeen  int
		byteSeen   int
		attachSeen int
		timedOut   bool
		turnErr    error
		sessionID  string
		stopReason string
		// toolsRun counts the distinct tool calls seen on the stream, so a turn
		// that ends without a reply can say what it did instead of guessing.
		toolsRun = map[string]struct{}{}
	)
	if h.sessionLog != nil {
		defer func() {
			// An agent failure returns through writer.Fail, which may itself
			// return nil, so retErr alone does not reveal it — turnErr captures
			// the agent error explicitly. Interrupt (ctx cancelled) and idle
			// timeout take precedence as they are not failures of the agent.
			outcome := turnCompleted
			switch {
			case errors.Is(context.Cause(ctx), context.Canceled), errors.Is(turnErr, context.Canceled):
				outcome = turnInterrupted
			case timedOut:
				outcome = turnTimedOut
			case nodeUnavailable(turnErr):
				outcome = turnNodeUnavailable
			case turnErr != nil || retErr != nil:
				outcome = turnErrored
			}
			// Capture the terminal error text for an errored turn so `journal
			// query` alone explains the failure — previously the real cause (e.g.
			// "start Slack stream: channel_type_not_supported") only existed in
			// daemon stderr. turnErr is the agent-reported cause; retErr covers a
			// delivery failure that Fail surfaced.
			errText := ""
			if outcome == turnErrored || outcome == turnNodeUnavailable {
				switch {
				case turnErr != nil:
					errText = turnErr.Error()
				case retErr != nil:
					errText = retErr.Error()
				}
			}
			// Fresh context: on the interrupt/timeout paths the request ctx is
			// already cancelled, but the row enqueue + transcript write must run.
			h.sessionLog.record(context.Background(), sessionTurn{
				req: req, agent: agentName, sessionID: sessionID, prompt: prompt,
				response: respBuf.String(), outcome: outcome, stopReason: stopReason,
				duration: time.Since(startedAt), chunks: chunkSeen, bytes: byteSeen,
				errText: errText,
			})
		}()
	}
	teamID, userID := req.TeamID, req.UserID
	if req.DM {
		teamID, userID = "", ""
	}
	streamOpts := StreamWriterOptions{
		ThreadTS: streamThreadTS, TeamID: teamID, UserID: userID,
		Interval: h.interval, MinChars: h.minChars, Logger: h.logger,
		TemplateDir: h.templateDir, ResolveUserName: h.userNames.Name,
	}
	renderer := h.newChatRenderer(req.ChannelID, streamThreadTS, streamOpts)
	// Tell the background events router where this conversation renders, so a subagent that
	// finishes after this turn ends is posted into the same thread. Keyed by the
	// deterministic session id — the same id claude_code fires OnBackground with.
	if h.backgroundEventsRouter != nil {
		h.backgroundEventsRouter.Register(agent.DeriveSessionID(metadata), bgTarget{
			channelID:  req.ChannelID,
			threadTS:   streamThreadTS,
			streamOpts: streamOpts,
		})
	}
	// Safety net: finalise every Slack message (text sections and tool blocks) on
	// any exit path. Declared first so it runs last (after the interrupt handler
	// below); the happy/error/idle paths finalise themselves, making this a no-op
	// except on an early return. Fresh context: the request ctx may already be
	// cancelled.
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		renderer.EnsureStopped(sctx)
	}()
	// Interrupt path: when the caller cancels ctx (gateway's interrupt closure or
	// the /stop slash command), render an "_interrupted_" marker instead of
	// bubbling the cancellation up as an error. Timeout-driven cancellations
	// (DeadlineExceeded) still surface as errors so operators notice stalls. Fresh
	// context, since the request ctx is cancelled on this path.
	//
	// retErr is checked too, because the backend can report the cancellation
	// before our own ctx carries it: the interrupt closure asks the session to
	// stop and only cancels ctx after a grace period, so the agent's aborted-turn
	// event routinely wins that race. Both are the same event — the turn was
	// cancelled — and must render the same marker.
	defer func() {
		if !errors.Is(context.Cause(ctx), context.Canceled) && !errors.Is(retErr, context.Canceled) {
			return
		}
		ictx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		renderer.Interrupted(ictx)
		retErr = nil
	}()
	// The assistant-threads "is thinking..." indicator is a thread-scoped Slack
	// feature, so it only applies when we are replying in a thread. In
	// channel-reply mode (empty streamThreadTS) there is no thread to attach it
	// to — skip it rather than emit a meaningless call that Slack rejects.
	if streamThreadTS != "" {
		if err := h.api.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{
			ChannelID: req.ChannelID,
			ThreadTS:  streamThreadTS,
			Status:    "is thinking...",
		}); err != nil {
			h.logger.Warn("failed to set assistant status", "error", err)
		}
		// Slack auto-clears the assistant status as soon as the first stream chunk
		// is sent (start/append/stop). Re-assert it periodically so the indicator
		// stays visible while the back-pressured stream is still being assembled.
		statusCtx, stopStatus := context.WithCancel(ctx)
		statusDone := make(chan struct{})
		go h.refreshAssistantStatus(statusCtx, req.ChannelID, streamThreadTS, "is thinking...", statusDone)
		defer func() {
			stopStatus()
			<-statusDone
			// Best-effort explicit clear so the indicator does not linger (e.g. when
			// the handler errors out before any chunk is sent, Slack would otherwise
			// keep "is thinking..." displayed for up to two minutes). Use a fresh
			// background context: on the interrupt path the request ctx is already
			// cancelled, and reusing it here would make Slack reject the clear and
			// leave "is thinking..." stuck on screen — the exact symptom we are
			// fixing.
			clearCtx, cancelClear := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelClear()
			if err := h.api.SetAssistantThreadsStatusContext(clearCtx, slack.AssistantThreadsSetStatusParameters{
				ChannelID: req.ChannelID,
				ThreadTS:  streamThreadTS,
				Status:    "",
			}); err != nil {
				h.logger.Debug("failed to clear assistant status", "error", err)
			}
		}()
	}
	// Drive the prompt under a child context we can cancel ourselves. The idle
	// watchdog uses it to unblock the in-flight ACP request without touching the
	// parent ctx — so on a timeout we can still render our own message on the
	// parent rather than tripping the interrupt path.
	promptCtx, cancelPrompt := context.WithCancel(ctx)
	defer cancelPrompt()
	events, err := sessions.Prompt(promptCtx, key, metadata, agent.PromptRequest{Text: promptForAgent, History: history})
	if err != nil {
		turnErr = err
		if errors.Is(err, agent.ErrCredentialRejected) {
			discardSession(sessions, key)
			h.logger.Warn("a node's claude_code credential was rejected on session start; the node asked its owner to sign in",
				"agent", route.Agent, "channel", req.ChannelID)
			return renderer.Fail(ctx, errOwnerSigningIn)
		}
		// A rejected credential surfaces here rather than as an event: Prompt opens
		// the session, and the launch handshake is where a bad credential is caught.
		// Drop the binding so the retry after re-authentication starts a fresh
		// process rather than reusing one that already failed to authenticate.
		if h.failedOnCredential(route.Agent, err) {
			discardSession(sessions, key)
			h.logger.Warn("claude_code credential rejected on session start; asked admin to re-authenticate",
				"agent", route.Agent, "channel", req.ChannelID)
			return renderer.Fail(ctx, errCredentialBlocked)
		}
		return renderer.Fail(ctx, err)
	}
	// The session now exists; capture its id for the turn record.
	if id, ok := sessions.Lookup(key); ok {
		sessionID = id
	}
	firstChunkLogged := false
	// finish resolves a successful turn: it finalises every open section and, when
	// the turn produced no reply text, surfaces an empty-reply note so silence is
	// legible. Shared by the explicit EventComplete and channel-closed paths.
	finish := func() error {
		var empty *alertcard.Spec
		// A turn that delivered a file but no prose is not empty — suppress the
		// note so an attachment-only reply does not look like a failed turn.
		if byteSeen == 0 && attachSeen == 0 {
			spec := emptyReplySpec(len(toolsRun))
			empty = &spec
			h.logger.Warn("agent turn completed with no agent text", "source", req.Source, "channel", req.ChannelID, "stop_reason", stopReason, "tools_run", len(toolsRun))
		}
		if err := renderer.Finish(ctx, empty); err != nil {
			return err
		}
		h.logger.Info("completed agent chat response", "source", req.Source, "channel", req.ChannelID, "duration", time.Since(startedAt), "chunks", chunkSeen, "bytes", byteSeen, "attachments", attachSeen, "stop_reason", stopReason)
		return nil
	}
	// Idle watchdog: a turn is bounded by inactivity, not total wall-clock. Every
	// event resets the timer; only an agent that goes silent for the whole window
	// trips it. A long turn that keeps emitting tool calls never times out.
	idle := time.NewTimer(h.effectiveIdleTimeout())
	defer idle.Stop()
	var held []heldEvent
	for {
		var event agent.Event
		ok := true
		if len(held) > 0 {
			event, ok = held[0].event, held[0].ok
			held = held[1:]
		} else {
			select {
			case <-idle.C:
				timedOut = true
				if sid, ok := sessions.Lookup(key); ok {
					cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					if cerr := sessions.Cancel(cancelCtx, sid); cerr != nil {
						h.logger.Warn("agent session cancel on idle timeout failed", "error", cerr, "session_id", sid)
					}
					cancel()
					discardSession(sessions, key)
				}
				renderer.EnsureStopped(ctx)
				h.postIdleAside(ctx, req.ChannelID, streamThreadTS)
				cancelPrompt()
				for range events {
				}
				h.logger.Warn("agent chat timed out on inactivity", "source", req.Source, "channel", req.ChannelID, "duration", time.Since(startedAt), "idle_timeout", h.effectiveIdleTimeout())
				return nil
			case event, ok = <-events:
			}
		}
		if !ok {
			return finish()
		}
		resetIdleTimer(idle, h.effectiveIdleTimeout())
		switch event.Type {
		case agent.EventText:
			if event.Text != "" {
				chunkSeen++
				byteSeen += len(event.Text)
				respBuf.WriteString(event.Text)
				if !firstChunkLogged {
					firstChunkLogged = true
					h.logger.Info("received first agent text chunk", "source", req.Source, "channel", req.ChannelID, "duration", time.Since(startedAt), "bytes", len(event.Text))
				}
			}
			if err := renderer.Text(ctx, event.Text); err != nil {
				return err
			}
		case agent.EventStatus:
			if event.Text != "" {
				h.logger.Debug("agent status", "source", req.Source, "channel", req.ChannelID, "status", event.Text)
			}
		case agent.EventTask:
			if event.Task == nil {
				continue
			}
			if event.Task.Kind != agent.TaskKindPlan && event.Task.ID != "" {
				toolsRun[event.Task.ID] = struct{}{}
			}
			if err := renderer.Task(ctx, event.Task); err != nil {
				return err
			}
		case agent.EventAttachment:
			if event.Attachment == nil {
				continue
			}
			if err := renderer.Attachment(ctx, event.Attachment); err != nil {
				h.logger.Warn("failed to deliver agent attachment", "source", req.Source, "channel", req.ChannelID, "filename", event.Attachment.Filename, "error", err)
			} else {
				attachSeen++
				h.logger.Info("delivered agent attachment", "source", req.Source, "channel", req.ChannelID, "filename", event.Attachment.Filename)
			}
		case agent.EventPermission:
			if event.Permission == nil {
				continue
			}
			decision := h.askPermission(ctx, req, route.Agent, streamThreadTS, renderer, event.Permission.Request)
			if event.Permission.Decision != nil {
				event.Permission.Decision <- decision
			}
		case agent.EventQuestion:
			if event.Question == nil {
				continue
			}
			answer, arrived := h.draw(ctx, renderer, events, func(cardCtx context.Context, d displayer, loc agent.TurnLocation) (agent.DisplayAnswer, error) {
				return d.Question(cardCtx, loc, event.Question.Request)
			}, req, streamThreadTS)
			held = append(held, arrived...)
			if event.Question.Answer != nil {
				event.Question.Answer <- answer
			}
			resetIdleTimer(idle, h.effectiveIdleTimeout())
		case agent.EventPlan:
			if event.Plan == nil {
				continue
			}
			answer, arrived := h.draw(ctx, renderer, events, func(cardCtx context.Context, d displayer, loc agent.TurnLocation) (agent.DisplayAnswer, error) {
				return d.Plan(cardCtx, loc, event.Plan.Request)
			}, req, streamThreadTS)
			held = append(held, arrived...)
			if event.Plan.Answer != nil {
				event.Plan.Answer <- answer
			}
			resetIdleTimer(idle, h.effectiveIdleTimeout())
		case agent.EventSignIn:
			if event.SignIn == nil {
				continue
			}
			held = append(held, h.drawSignIn(ctx, renderer, events, event.SignIn, req, streamThreadTS)...)
			resetIdleTimer(idle, h.effectiveIdleTimeout())
		case agent.EventError:
			turnErr = event.Error
			if errors.Is(event.Error, context.Canceled) || errors.Is(context.Cause(ctx), context.Canceled) {
				return event.Error
			}
			if errors.Is(event.Error, agent.ErrCredentialRejected) {
				discardSession(sessions, key)
				h.logger.Warn("a node's claude_code credential was rejected mid-turn; the node asked its owner to sign in",
					"agent", route.Agent, "channel", req.ChannelID, "session_id", sessionID)
				return renderer.Fail(ctx, errOwnerSigningIn)
			}
			if h.failedOnCredential(route.Agent, event.Error) {
				discardSession(sessions, key)
				h.logger.Warn("claude_code credential rejected mid-turn; asked admin to re-authenticate",
					"agent", route.Agent, "channel", req.ChannelID, "session_id", sessionID)
				return renderer.Fail(ctx, errCredentialBlocked)
			}
			if errors.Is(event.Error, agent.ErrToolCeiling) {
				discardSession(sessions, key)
				h.logger.Warn("dropped agent session after tool ceiling", "source", req.Source, "channel", req.ChannelID, "session_id", sessionID)
			}
			return renderer.Fail(ctx, event.Error)
		case agent.EventComplete:
			stopReason = event.StopReason
			return finish()
		}
	}
}

// askPermission resolves an ACP permission request, returning the chosen option's
// ID ("" denies). It first settles any open reply section through the renderer so
// the out-of-band approval card posts below a committed message rather than an
// in-flight stream, then asks the human via the wired gate in the turn's own
// thread. A nil gate (no human wired) or an ask error denies — the turn must not
// hang waiting on an answer that can never come.
func (h *ChatHandler) askPermission(ctx context.Context, req ChatRequest, agentName, threadTS string, renderer chatRenderer, pr agent.PermissionRequest) string {
	renderer.BeginInterjection(ctx)
	asker := h.permissionAskers[agentName]
	if asker == nil {
		h.logger.Warn("agent permission request but no asker wired; denying", "channel", req.ChannelID, "agent", agentName, "tool_kind", pr.ToolKind)
		return ""
	}
	loc := agent.TurnLocation{ChannelID: req.ChannelID, ThreadTS: threadTS, UserID: req.UserID}
	optionID, err := asker.AskPermission(ctx, loc, pr)
	if err != nil {
		h.logger.Warn("agent permission ask failed; denying", "channel", req.ChannelID, "error", err)
		return ""
	}
	return optionID
}

type heldEvent struct {
	event agent.Event
	ok    bool
}

func (h *ChatHandler) draw(ctx context.Context, renderer chatRenderer, events <-chan agent.Event, ask func(context.Context, displayer, agent.TurnLocation) (agent.DisplayAnswer, error), req ChatRequest, threadTS string) (agent.DisplayAnswer, []heldEvent) {
	renderer.BeginInterjection(ctx)
	if h.display == nil {
		h.logger.Warn("agent raised a question or plan but nothing can draw it", "channel", req.ChannelID)
		return agent.DisplayAnswer{Outcome: agent.DisplayUnavailable}, nil
	}
	type drawn struct {
		answer agent.DisplayAnswer
		err    error
	}
	cardCtx, withdraw := context.WithCancel(ctx)
	defer withdraw()
	done := make(chan drawn, 1)
	go func() {
		answer, err := ask(cardCtx, h.display, agent.TurnLocation{ChannelID: req.ChannelID, ThreadTS: threadTS, UserID: req.UserID})
		done <- drawn{answer, err}
	}()

	var held []heldEvent
	for {
		var result drawn
		select {
		case result = <-done:
		case ev, ok := <-events:
			held = append(held, heldEvent{event: ev, ok: ok})
			if ok && ev.Type != agent.EventError && ev.Type != agent.EventComplete {
				continue
			}
			withdraw()
			result = <-done
		}
		if result.err != nil {
			h.logger.Warn("could not draw an agent's question or plan", "channel", req.ChannelID, "error", result.err)
			return agent.DisplayAnswer{Outcome: agent.DisplayUnavailable, Note: result.err.Error()}, held
		}
		return result.answer, held
	}
}

func (h *ChatHandler) drawSignIn(ctx context.Context, renderer chatRenderer, events <-chan agent.Event, prompt *agent.SignInPrompt, req ChatRequest, threadTS string) []heldEvent {
	renderer.BeginInterjection(ctx)
	if h.display == nil {
		h.logger.Warn("agent raised a sign-in but nothing can draw it", "channel", req.ChannelID, "tool", prompt.Request.Tool)
		select {
		case prompt.Answer <- agent.DisplayAnswer{Outcome: agent.DisplayUnavailable}:
		default:
		}
		return nil
	}
	cardCtx, withdraw := context.WithCancel(ctx)
	defer withdraw()
	settled := make(chan agent.SignInSettled)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.display.SignIn(cardCtx, agent.TurnLocation{ChannelID: req.ChannelID, ThreadTS: threadTS, UserID: req.UserID}, prompt, settled)
	}()

	var held []heldEvent
	for {
		select {
		case <-done:
			return held
		case ev, ok := <-events:
			if ok && ev.SignInSettled != nil && ev.SignInSettled.Prompt == prompt {
				select {
				case settled <- *ev.SignInSettled:
				case <-done:
					return held
				}
				continue
			}
			held = append(held, heldEvent{event: ev, ok: ok})
			if ok && ev.Type != agent.EventError && ev.Type != agent.EventComplete {
				continue
			}
			withdraw()
			<-done
			return held
		}
	}
}

func (h *ChatHandler) refreshAssistantStatus(ctx context.Context, channelID, threadTS, status string, done chan<- struct{}) {
	defer close(done)
	interval := h.statusRefreshInterval
	if interval <= 0 {
		interval = defaultStatusRefreshInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := h.api.SetAssistantThreadsStatusContext(ctx, slack.AssistantThreadsSetStatusParameters{
				ChannelID: channelID,
				ThreadTS:  threadTS,
				Status:    status,
			}); err != nil && !errors.Is(err, context.Canceled) {
				h.logger.Debug("failed to refresh assistant status", "error", err)
			}
		}
	}
}

// conversationKey identifies the agent session a request belongs to. A threaded
// conversation — channel thread OR DM — is bound to its thread, so a session is
// never shared across threads: the key is the request's ThreadTS, falling back
// to its own MessageTS for a top-level message that roots a new thread
// (mirroring replyThreadTS, so the key matches where replies are posted).
//
// In channel-reply mode (replyOnThread=false on a top-level message) there is no
// thread to bind to, and binding per-MessageTS would spawn a fresh session every
// message — shredding context. Instead ThreadTS stays empty, so every top-level
// message in the channel maps to the SAME key {ChannelID, ThreadTS:""}: one
// long-lived, channel-wide rolling conversation. The key omits UserID, so that
// session is shared by the channel's participants. An explicit reset is /clear,
// not an implicit per-message one.
//
// The DM flag is retained so a DM thread and a same-id channel thread cannot
// collide and so session metadata/logging still distinguishes the two surfaces.
func conversationKey(req ChatRequest, replyOnThread bool) agent.ConversationKey {
	threadTS := req.ThreadTS
	if threadTS == "" && replyOnThread {
		threadTS = req.MessageTS
	}
	return agent.ConversationKey{TeamID: req.TeamID, ChannelID: req.ChannelID, ThreadTS: threadTS, DM: req.DM}
}

// isTerminalTaskStatus reports whether an ACP task status is a final outcome.
// A task in any other state — pending, in_progress, or an update that omitted
// its status — is still running and must stay tracked so it is finalised
// rather than abandoned mid-flight.
func isTerminalTaskStatus(status agent.TaskStatus) bool {
	switch status {
	case agent.TaskStatusComplete, agent.TaskStatusFailed, agent.TaskStatusCancelled:
		return true
	default:
		return false
	}
}

// replyThreadTS decides where the bot's reply is posted. An incoming message
// that is ALREADY in a thread always gets a threaded reply (never yank a
// threaded conversation out to the channel root). For a top-level message,
// replyOnThread selects the strategy: true roots a thread at the message
// (historical behaviour), false posts directly at the channel root (empty
// thread_ts). Mirrors conversationKey so the reply lands where the session lives.
func replyThreadTS(req ChatRequest, replyOnThread bool) string {
	if req.ThreadTS != "" {
		return req.ThreadTS
	}
	if !replyOnThread {
		return ""
	}
	return req.MessageTS
}
