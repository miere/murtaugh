package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agent/native"
	"github.com/miere/murtaugh/internal/agentbuild"
	"github.com/miere/murtaugh/internal/agentdelegate"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/journal"
	"github.com/miere/murtaugh/internal/mcpbridge"
	"github.com/miere/murtaugh/internal/slack/approvalcard"
	"github.com/miere/murtaugh/internal/slack/askcard"
	"github.com/miere/murtaugh/internal/slack/authcard"
	slackclient "github.com/miere/murtaugh/internal/slack/client"
	askbroker "github.com/miere/murtaugh/internal/slack/interaction"
	"github.com/miere/murtaugh/internal/tools"
	"github.com/miere/murtaugh/internal/toolset"
	"github.com/miere/murtaugh/internal/unfurl"
	"github.com/miere/murtaugh/internal/updates"
	"github.com/miere/murtaugh/internal/workflow"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// workflowDispatcher is the minimal surface needed to dispatch an interactive
// callback to a workflow engine and wire the chat pipeline its
// delegate-to-agent triggers start turns on. *workflow.Engine satisfies it.
type workflowDispatcher interface {
	Execute(ctx context.Context, interaction slack.InteractionCallback, rawPayload []byte) error
	SetChatStarter(workflow.ChatStarter)
}

// RestartTrigger is the function the Gateway calls to request a graceful
// restart. The arguments mirror internal/app.RestartRequest field-by-field
// but stay stringly-typed so gateway does not need to import the
// composition root (which would create a cycle). Returns true when the
// shutdown sequence has begun, false when the request was declined
// (already firing, within cool-down, or no coordinator is wired).
type RestartTrigger func(source, userID, channel, reason string) bool

// TroubleshootBundler assembles a diagnostics bundle described by note and
// returns the path to the written zip plus any non-fatal collection warnings.
// The gateway owns Slack delivery; the composition root supplies this closure
// over the deterministic troubleshoot bundler. nil disables the
// `/murtaugh troubleshoot` slash command.
type TroubleshootBundler func(ctx context.Context, note string) (zipPath string, warnings []string, err error)

type Gateway struct {
	api      userDirectoryAPI
	socket   *socketmode.Client
	handler  SlashCommandHandler
	workflow workflowDispatcher
	// interactions routes broker prompt clicks (the `ask` tool, later the
	// approval gate) back to the blocked turn. nil leaves broker prompts
	// unrouted (CLI/MCP, or a gateway built without it).
	interactions *askbroker.Broker

	// auth routes `auth.request` card clicks and code submissions back to the
	// blocked request. nil leaves auth requests unroutable, which the tool
	// reports rather than hanging.
	auth *authcard.Flow
	// askCards routes ask-card clicks back into the blocked `ask` tool call.
	askCards *askcard.Flow
	// bridge is the shared per-agent MCP aggregator. ACP agents are handed a
	// `murtaugh mcp-bridge` stdio server that proxies to it, so they can reach
	// Murtaugh's own tools. nil when ACP chat is disabled. Started in Run.
	bridge          *mcpbridge.Server
	chat            *ChatHandler
	chatSessions    map[string]ChatSessionManager
	chatWarmTimeout time.Duration
	// chatRouting and agentProfiles are config snapshots captured at construction
	// so the startup routing summary (logStartupRouting) can report the configured
	// agents and channel routing — and flag routes whose target agent failed to
	// build — without re-reading the full config.
	chatRouting   config.ChatConfig
	agentProfiles map[string]config.AgentProfile
	// agentToolProblems records, per agent, the workdir-rooted tool groups that
	// were dropped at build time because the agent had no resolvable workspace.
	// The agent still answers; the startup summary reports each dropped feature.
	agentToolProblems map[string][]toolset.Problem
	// cancelGrace is how long the interrupt path waits after asking the
	// ACP agent to cancel its in-flight prompt before hard-cancelling the
	// chat goroutine's context. Short enough that the user does not stare
	// at a stalled "_interrupted_" marker, long enough that trailing
	// chunks already on the wire can flush. Defaults to 2s via
	// ACPConfig.EffectiveCancelGracePeriod.
	cancelGrace time.Duration
	inFlight    *InFlightRegistry
	// coalescer serialises a conversation's turns and merges rapid-fire or
	// mid-turn follow-ups into a single coalesced turn (replaces the older
	// interrupt-and-replace path). Wired in New once the Gateway exists.
	coalescer *coalescer
	// recentEvents suppresses duplicate Slack event deliveries so a
	// redelivered message does not spawn a second chat that interrupts the
	// first. nil disables de-duplication (CLI/MCP and most tests).
	recentEvents    *eventDedup
	unfurl          *LinkUnfurlHandler
	unfurlTimeout   time.Duration
	startupNotifier StartupNotifier
	logger          *slog.Logger
	// cfg holds the configuration values consulted at runtime. Authz entries
	// (admin_user, allowed_users) start out as configured (IDs or handles) and
	// are mutated in place by resolveAllowSet at the start of Run so the rest
	// of the Gateway can rely on ID-only comparisons via cfg.IsAllowedUser.
	cfg config.AccessConfig
	// restart is the optional graceful-restart trigger. nil in CLI/MCP and
	// in tests that do not need to exercise the restart path; the slash
	// handler reports "not available" when nil.
	restart RestartTrigger
	// version is the running binary's compile-time version, shown on the App
	// Home control panel. Empty in tests and struct-literal gateways.
	version string
	// updates reports whether a newer Murtaugh release is available; consulted
	// when rendering the admin's App Home panel. nil disables the update check,
	// so the panel shows the version alone.
	updates *updates.Checker
	// installUpdate downloads and swaps in the binary for a given release tag,
	// returning the installed version. Injected over the setup.update tool. nil
	// disables the App Home "Update" action (the button still never appears
	// unless updates is also wired).
	installUpdate func(ctx context.Context, target string) (string, error)
	// resumeStore persists the restart marker between processes. nil
	// disables the "restarting…" / "back online" Slack confirmation flow
	// — the restart still happens, just silently.
	resumeStore ResumeMarkerStore
	// messaging is the Slack surface used by the resume helpers. Set to
	// the same *slack.Client as api in New; kept as a separate field so
	// tests can substitute a narrow fake without re-implementing the
	// full Slack client.
	messaging slackMessagingAPI
	// connectHandled flips to true the first time the connect-time greeting
	// runs after a successful socket connect. Slack may emit multiple
	// EventTypeConnected events across the daemon's life (re-connects, flaky
	// links); we only want to greet — resume notice or startup ping — once per
	// process. See notifyConnected.
	connectHandled bool
	// scheduledJobs is the job set captured from the loaded config at
	// construction. The scheduler registers the entries whose ScheduleKind
	// is cron/every; manual jobs are ignored. Empty disables scheduling.
	scheduledJobs map[string]config.JobProfile
	// confirmedJobs records which held (agent-defined, unconfirmed) jobs have
	// been approved for their first run during this process. Guarded by
	// confirmedJobsMu. It is the in-process half of the approval: persistJobConfirmation
	// writes the durable half so a restart does not re-ask.
	confirmedJobs   map[string]bool
	confirmedJobsMu sync.Mutex
	// persistJobConfirmation stamps an approved job `confirmed: true` in the
	// config store, so the approval outlives this process. Wired by the
	// composition root (WithJobConfirmer) as a closure over the store. nil
	// keeps the approval session-scoped — the job still runs now, but a
	// restart re-asks. Safe to leave nil in CLI/MCP and tests.
	persistJobConfirmation JobConfirmer
	// runJob executes a job by name to completion. Injected by the
	// composition root (WithScheduledRunner) as a closure over the jobs.run
	// tool. nil disables the scheduler, so CLI/MCP and tests never pay for
	// it.
	runJob ScheduledRunner
	// recorder receives gateway-stream journal events for inbound interactions
	// (slash commands, interactive callbacks) and is threaded into the workflow
	// engine and unfurl handler. Never nil after New: a nil argument becomes a
	// no-op recorder so call sites never branch.
	recorder journal.Recorder
	// troubleshoot assembles a diagnostics bundle for the
	// `/murtaugh troubleshoot` slash command. nil disables the command.
	troubleshoot TroubleshootBundler
	// botToken is the bot OAuth token, retained so the troubleshoot handler can
	// build a Slack client that uploads the bundle to the admin DM (the narrow
	// api/messaging interfaces deliberately do not expose file upload).
	botToken string
	// journalSweep runs one retention pass over the journal; journalSweepEvery
	// is its cadence. Wired by the composition root (WithJournalSweeper) as a
	// closure over the daemon's store. nil disables the sweeper, so CLI/MCP and
	// tests never start it.
	journalSweep      func(context.Context) error
	journalSweepEvery time.Duration
	// webClient is the concrete Slack Web API client. It backs both the
	// active connection heartbeat (auth.test) and the construction of fresh
	// socketmode clients on every (re)connect. The Web API is stateless HTTP
	// and never goes "zombie", so the same client is reused across reconnects.
	// nil in struct-literal test gateways, which never run the supervisor.
	webClient *slack.Client
	// connMu guards socket across the supervisor's reconnects: the supervisor
	// swaps in a fresh socketmode.Client per attempt while the ack path reads
	// the current one. The single *socketmode.Client field is otherwise written
	// once at construction.
	connMu sync.Mutex
	// lastActivityNano is the UnixNano of the most recent inbound socketmode
	// event of ANY kind. The watchdog reads it to detect a half-open websocket
	// (the daemon believes it is connected but no frames arrive); the event loop
	// stamps it. Atomic so the watchdog goroutine reads it without the lock.
	lastActivityNano atomic.Int64
	// now supplies the current time; overridable in tests. nil ⇒ time.Now.
	now func() time.Time
	// channelCache maps Slack channel IDs to names so the chat resolver can
	// route channel→agent by NAME glob (chat.channel_agents) without doing any
	// Slack API I/O on the socket goroutine. Warmed at startup and refreshed on
	// a ticker by startChannelCache; nil disables name-based routing (only the
	// exact channel-ID keys still match), so CLI/MCP and tests never pay for it.
	channelCache *channelNameCache
	// noMentionPerChannel maps a channel-ID/channel-NAME glob (same key syntax as
	// chat.channel_agents) to the Slack user IDs whose plain channel messages the
	// bot replies to without an @mention. Captured from cfg.Chat at construction;
	// any handle entries are resolved to IDs by resolveAllowSet at the start of
	// Run, so the runtime membership test in handleEventsAPI is ID-only. The
	// effective no-mention set for a channel is the UNION of noMentionEverywhere
	// and the values of every pattern whose glob matches the channel.
	noMentionPerChannel map[string][]string
	// noMentionEverywhere is the global no-mention list (chat.no_mention.everywhere).
	// Captured from cfg.Chat at construction and resolved to IDs by resolveAllowSet.
	noMentionEverywhere []string
	// chatChannels is the ordered chat.channels rule list, captured at
	// construction. handleEventsAPI consults it for the allow_anyone waiver;
	// the chat resolver consults cfg.Chat.Channels for routing. Both use
	// matchChannel, so a channel is judged by exactly one rule either way.
	chatChannels config.ChannelRules
}

func New(cfg config.Config, registry *tools.Registry, logger *slog.Logger, recorder journal.Recorder, broker *askbroker.Broker, authFlow *authcard.Flow, askFlow *askcard.Flow) *Gateway {
	if logger == nil {
		logger = slog.Default()
	}
	if recorder == nil {
		recorder = journal.NopRecorder{}
	}
	api := slack.New(cfg.OAuth.BotToken, slack.OptionAppLevelToken(cfg.OAuth.AppToken))
	socket := socketmode.New(api, socketmode.OptionDebug(cfg.Access.Debug))
	startupNotifier, err := NewSlackStartupNotifier(api, cfg.Access.AdminUser, logger)
	if err != nil {
		logger.Error("startup Slack ping disabled", "error", err)
	}
	// The channel-name cache backs name-glob routing in chat.channel_agents. It
	// needs a SlackAPI with ListChannels (the narrow socket api/webClient do
	// not expose it), so build a Web API client over the bot token. A build
	// failure (empty token — already rejected by config validation) just leaves
	// the cache nil, degrading to exact channel-ID matching.
	var channelCache *channelNameCache
	if channelAPI, err := slackclient.NewClient(cfg.OAuth.BotToken); err != nil {
		logger.Warn("channel-name routing disabled: could not build Slack client", "error", err)
	} else {
		// api (a *slack.Client) provides conversations.info for the synchronous
		// resolve-on-miss that lets canvas turns route by their channel, and
		// files.info for the canvas→parent-channel hop a file-backed canvas
		// conversation needs (its own name identifies the file, not the channel).
		channelCache = newChannelNameCache(channelAPI, api, slackCanvasParent{api: api}, 30*time.Second, logger)
	}

	var chat *ChatHandler
	var sessions map[string]ChatSessionManager
	// agentToolProblems records tool groups dropped while building each agent (a
	// degraded feature, not a failed agent) so the startup summary can surface
	// them in logs and the journal.
	agentToolProblems := make(map[string][]toolset.Problem)
	if !cfg.Chat.Enabled {
		logger.Warn("chat disabled: set chat.enabled: true to enable DM and app_mention replies (delegation still runs)")
	}
	var bridge *mcpbridge.Server
	var bgRouter *backgroundEventsRouter
	if cfg.Chat.Enabled {
		sessions = make(map[string]ChatSessionManager)
		// Renders claude_code background completions (subagents finishing after a
		// turn ends) into their thread; shared across agents, bound to the chat
		// handler's renderer below.
		bgRouter = newBackgroundEventsRouter(logger)
		// The aggregator lets ACP agents reach Murtaugh's own tools over a private
		// socket; built here, bound and torn down in Run. ACP agents that fail to
		// reach it simply get no Murtaugh tools.
		bridge = mcpbridge.NewServer(bridgeSocketPath(), logger)
		// Chat agents are gated: a side-effecting tool call asks the user for
		// approval in the thread. nil broker leaves them ungated. Headless and
		// delegated agents (built elsewhere) never get an approver.
		//
		// Both gates are built per agent, inside the loop below, because each
		// carries that agent's approval settings — approval.keep_resolved decides
		// whether its settled cards are kept or swept, and one shared gate could
		// only ever honour one agent's choice. profile.Approval is already the
		// resolved policy: both config loaders bake defaults.approval into every
		// agent before this point.
		//
		// One renderer serves all of them: it is stateless, and it reads card
		// templates from the config dir first so an operator can restyle them
		// without a rebuild, falling back to the embedded assets tree.
		approvalCards := approvalcard.NewRenderer(cfg.BaseDir, assets.FS)
		acpPermissionAskers := make(map[string]agent.PermissionAsker, len(cfg.Agents))
		for name, profile := range cfg.Agents {
			var approver native.Approver
			if broker != nil {
				keepResolved := profile.Approval.KeepsResolved()
				// One always-allow set per agent, shared by both of that agent's
				// approval paths: the gate below records a grant, and the permission
				// gate honours it rather than re-asking about a call the user has
				// already allowed through Murtaugh's own tools.
				grants := askbroker.NewGrants()
				approver = askbroker.NewApprover(broker, approvalCards, keepResolved, grants)
				// ACP agents' permission requests are resolved through the same broker:
				// the ACP client raises an EventPermission on the turn's event stream and
				// the chat handler asks here, posting the card in the thread (ordered with
				// the reply, like the native approver). A missing entry (headless) leaves
				// ACP agents to their auto-allow/deny policy. Set only on this
				// interactive path.
				acpPermissionAskers[name] = askbroker.NewPermissionGate(broker, approvalCards, keepResolved, grants)
			}
			// Resolve the agent's workspace once (workdir → base dir fallback),
			// validated here at the build seam. Any workdir-rooted tool that
			// cannot be rooted is dropped (degraded) rather than failing the
			// agent; the dropped features are surfaced at startup below.
			resolved, err := agentbuild.Resolve(name, profile, cfg.BaseDir)
			if err != nil {
				logger.Error("agent disabled: could not resolve agent", "agent", name, "kind", profile.ResolvedKind(), "error", err)
				continue
			}
			agentWorkDir := resolved.Dir()
			if problems := resolved.Problems(); len(problems) > 0 {
				agentToolProblems[name] = problems
				for _, p := range problems {
					logger.Warn("agent tool disabled", "agent", name, "tool", p.Group, "reason", p.Reason)
				}
			}
			// Mirror the bundled skills this agent opted to export into its
			// workdir so a filesystem-discovering backend can load them; the
			// default (empty) leaves them in-binary only. Non-fatal: a failure
			// just means no filesystem skills for this agent. Skipped when the
			// agent has no workspace (nothing to export into).
			if agentWorkDir != "" {
				if exported, err := config.ReconcileExportedSkills(agentWorkDir, profile.ExportSkillsToFS); err != nil {
					logger.Warn("skill export failed", "agent", name, "error", err)
				} else if len(exported) > 0 {
					logger.Info("exported bundled skills to workdir", "agent", name, "skills", exported, "dir", filepath.Join(agentWorkDir, ".agents", "skills"))
				}
			}
			client, err := agentbuild.Client(resolved, agentbuild.Deps{
				Registry:               registry,
				MCPServers:             cfg.MCPServers,
				WorkspaceDir:           cfg.BaseDir,
				Logger:                 logger.With("agent", name),
				Approver:               approver,
				Bridge:                 bridge,
				LongRunningToolTimeout: cfg.Defaults.EffectiveLongRunningToolTimeout(),
				BackgroundSink:         bgRouter.Handle,
			})
			if err != nil {
				logger.Error("agent disabled: could not build client", "agent", name, "kind", profile.ResolvedKind(), "error", err)
				continue
			}
			var interruptible *bool
			if profile.ACP != nil {
				interruptible = profile.ACP.Interruptible
			}
			sessions[name] = agent.NewSessionManager(
				client,
				cfg.Defaults.EffectiveSessionIdleTimeout(),
				cfg.Defaults.EffectiveMaxSessions(),
			).WithLogger(logger.With("agent", name)).
				WithCancelOverride(interruptible).
				WithDescriptor(string(profile.ResolvedKind()), profile.ResolvedApproval())
		}

		// The resolver runs on the Slack socket goroutine, so it must not do any
		// Slack API I/O: channel→agent routing consults the in-memory
		// channelCache (ID→name) and the pure matchChannel matcher. A name
		// the cache has not learned yet (a brand-new channel) falls back to the
		// default agent and triggers a non-blocking refresh so the NEXT message
		// can match by name.
		resolver := func(req ChatRequest) ChatRoute {
			// An explicit override (a delegate-to-agent trigger that named an
			// agent) wins over channel routing; empty falls through to it.
			applyOverride := func(r ChatRoute) ChatRoute {
				if req.AgentOverride != "" {
					r.Agent = req.AgentOverride
				}
				return r
			}
			def := cfg.Chat.Defaults
			if req.DM {
				agent := def.DMAgent
				if agent == "" {
					agent = def.Agent
				}
				// DMs live in the assistant pane, which is inherently threaded, so
				// the reply strategy does not apply to them.
				return applyOverride(ChatRoute{Agent: agent, ReplyOnThread: true})
			}
			channelName, known := channelCache.nameFor(req.ChannelID)
			if !known {
				channelCache.refreshAsync(context.Background())
			}
			route := ChatRoute{Agent: def.Agent, ReplyOnThread: def.EffectiveReplyOnThread()}
			if cc, ok := matchChannel(req.ChannelID, channelName, cfg.Chat.Channels); ok {
				if cc.Agent != "" {
					route.Agent = cc.Agent
				}
				if cc.ReplyOnThread != nil {
					route.ReplyOnThread = *cc.ReplyOnThread
				}
			}
			return applyOverride(route)
		}

		// Record chat turns to the acp_session stream only when it is enabled,
		// so a disabled stream writes neither rows nor transcript files.
		var sessionLog *sessionLogger
		if cfg.Journal.EffectiveEnabled(journal.StreamACPSession) {
			sessionLog = newSessionLogger(recorder, cfg.Journal.EffectiveBlobDir(cfg.BaseDir, cfg.BaseName), logger)
		}
		// Resolve this bot's own Slack user id once so thread backfill can mark
		// the agent's prior replies as its own. Best-effort: a failed auth.test
		// only costs the "(you)" tagging, not the backfill itself.
		var botUserID string
		authCtx, cancelAuth := context.WithTimeout(context.Background(), 10*time.Second)
		if resp, err := api.AuthTestContext(authCtx); err != nil {
			logger.Warn("auth.test failed; thread backfill will not tag the bot's own replies", "error", err)
		} else {
			botUserID = resp.UserID
		}
		cancelAuth()

		chat = NewChatHandler(
			api,
			sessions,
			resolver,
			cfg.Defaults.EffectiveStreamAppendInterval(),
			cfg.Defaults.EffectiveStreamMinChunkChars(),
			logger,
		).WithIdleTimeout(cfg.Defaults.EffectiveRequestTimeout()).WithSessionLogger(sessionLog).
			WithProgressDisplay(cfg.EffectiveProgressDisplay).WithStatusMessenger(api).
			WithBackfiller(NewThreadBackfiller(api, botUserID, logger)).
			WithCanvasInfo(slackCanvasInfo{api: api}).
			WithFileFetcher(api).
			WithUploader(slackAttachmentUploader{api: api}).
			WithPermissionAskers(acpPermissionAskers).
			WithReplyBlocks(cfg.BaseDir, api).
			WithBackgroundEventsRouter(bgRouter)
		// The router renders background turns through the chat handler's own renderer,
		// so a background reply looks exactly like a foreground one.
		bgRouter.bind(chat.newChatRenderer)
	}
	// One shared runner backs every delegate-to-agent surface (jobs, workflow
	// triggers, unfurls). Each delegation spins its own isolated agent process,
	// so this is safe to share. Built only when agents are configured; config
	// validation guarantees any delegate-to-agent rule names a known agent.
	var unfurlDelegator UnfurlDelegator
	var workflowDelegator workflow.AgentDelegator
	if len(cfg.Agents) > 0 {
		runner := agentdelegate.NewRunner(cfg.Agents, cfg.Defaults, cfg.BaseDir, logger).
			WithBuildContext(registry, cfg.MCPServers)
		unfurlDelegator = runner
		workflowDelegator = runner
	}
	var unfurlHandler *LinkUnfurlHandler
	if len(cfg.UnfurlRules) > 0 {
		matcher, err := unfurl.NewMatcher(cfg.UnfurlRules)
		if err != nil {
			logger.Error("custom link unfurling disabled", "error", err)
		} else {
			renderer := unfurl.NewRenderer(cfg.BaseDir, nil)
			unfurlHandler = NewLinkUnfurlHandler(matcher, renderer, nil, unfurlDelegator, api, logger).WithRecorder(recorder)
		}
	}
	g := &Gateway{
		api:               api,
		webClient:         api,
		socket:            socket,
		now:               time.Now,
		handler:           NewDefaultSlashCommandHandler(),
		workflow:          workflow.NewEngine(cfg, workflow.Options{Logger: logger, Delegator: workflowDelegator, Recorder: recorder}),
		interactions:      broker,
		auth:              authFlow,
		askCards:          askFlow,
		bridge:            bridge,
		chat:              chat,
		chatSessions:      sessions,
		chatRouting:       cfg.Chat,
		agentProfiles:     cfg.Agents,
		agentToolProblems: agentToolProblems,
		chatWarmTimeout:   cfg.Defaults.EffectiveStartupTimeout(),
		cancelGrace:       cfg.Defaults.EffectiveCancelGracePeriod(),
		inFlight:          NewInFlightRegistry(),
		recentEvents:      newEventDedup(0),
		unfurl:            unfurlHandler,
		unfurlTimeout:     2 * time.Minute,
		startupNotifier:   startupNotifier,
		logger:            logger,
		cfg:               cfg.Access,
		messaging:         api,
		scheduledJobs:     cfg.Jobs,
		recorder:          recorder,
		botToken:          cfg.OAuth.BotToken,
		channelCache:      channelCache,
		// Captured here so the no-mention check in handleEventsAPI runs without
		// re-importing the full cfg.
		noMentionPerChannel: cfg.Chat.NoMention.ByChannel,
		noMentionEverywhere: cfg.Chat.NoMention.Everywhere,
		chatChannels:        cfg.Chat.Channels,
	}
	// The coalescer needs g's dispatch/interrupt hooks, so it is wired after the
	// struct exists. It owns the decision of when and what to dispatch per
	// conversation; startChat merely submits each message to it.
	g.coalescer = newCoalescer(
		defaultCoalesceWindow,
		nil, // production timer (time.AfterFunc)
		g.agentInterruptible,
		g.inFlight.Cancel,
		g.dispatchTurn,
		logger,
	)
	// A top-level delegate-to-agent trigger starts a real chat turn through the
	// gateway itself. Only wire it when chat is live; left unset, such a trigger
	// reports that chat is required rather than dereferencing a nil pipeline.
	if chat != nil {
		g.workflow.SetChatStarter(g)
	}
	return g
}

// StartChat satisfies workflow.ChatStarter: it turns a delegated chat spec into
// the gateway's normal chat request and dispatches it on the shared pipeline
// (streaming, journaling, approval gate, per-thread binding). It returns once
// the turn is dispatched; the turn itself runs asynchronously, exactly like an
// @mention. A nil chat pipeline (ACP disabled) is reported, not dereferenced.
func (a *Gateway) StartChat(ctx context.Context, spec workflow.ChatSpec) error {
	if a.chat == nil {
		return errors.New("chat is disabled")
	}
	// Detach from the interaction handler's context. That context is cancelled
	// the instant the (now non-blocking) trigger dispatch returns, which would
	// otherwise kill the turn we just started; the @mention path likewise runs
	// on a background context. Carry the correlation id across so the turn's
	// journal events still tie back to this click.
	turnCtx := journal.WithCorrID(context.Background(), journal.CorrIDFromContext(ctx))
	a.startChat(turnCtx, ChatRequest{
		TeamID:        spec.TeamID,
		ChannelID:     spec.ChannelID,
		ThreadTS:      spec.ThreadTS,
		UserID:        spec.UserID,
		Text:          spec.Text,
		Source:        spec.Source,
		AgentOverride: spec.Agent,
	})
	return nil
}

// record emits a gateway-stream journal event, stamping the correlation id
// carried on ctx (minted at interaction ingress). A nil recorder (struct-literal
// Gateways in tests) is a no-op, matching how the gateway treats its other
// optional collaborators.
func (a *Gateway) record(ctx context.Context, kind string, level journal.Level, summary string, keys journal.Keys, payload any) {
	if a.recorder == nil {
		return
	}
	a.recorder.Record(ctx, journal.Event{
		Stream:  journal.StreamGateway,
		Kind:    kind,
		Level:   level,
		Summary: summary,
		CorrID:  journal.CorrIDFromContext(ctx),
		Keys:    keys,
		Payload: payload,
	})
}

// WithResumeMarkerStore attaches the persistent store used to bridge
// restart notices across process restarts. When nil (the default) the
// restart flow runs silently — the "restarting…" / "back online"
// confirmation messages are skipped.
func (a *Gateway) WithResumeMarkerStore(store ResumeMarkerStore) *Gateway {
	a.resumeStore = store
	return a
}

// WithRestartTrigger attaches the graceful-restart trigger and returns the
// receiver for fluent wiring at the composition root. When nil (the
// default) the restart slash verb reports the feature as unavailable.
func (a *Gateway) WithRestartTrigger(trigger RestartTrigger) *Gateway {
	a.restart = trigger
	return a
}

// WithVersion records the running binary's compile-time version for display on
// the App Home control panel. Blank (the default) renders as "unknown".
func (a *Gateway) WithVersion(version string) *Gateway {
	a.version = strings.TrimSpace(version)
	return a
}

// WithUpdateChecker wires the App Home update affordance: checker reports
// whether a newer release exists, and install downloads+swaps the binary for a
// given tag (returning the installed version). Passing nil for either leaves
// the panel showing the version without an "Update" button.
func (a *Gateway) WithUpdateChecker(checker *updates.Checker, install func(ctx context.Context, target string) (string, error)) *Gateway {
	a.updates = checker
	a.installUpdate = install
	return a
}

// WithScheduledRunner attaches the executor used to run cron/every-scheduled
// jobs and returns the receiver for fluent wiring. When nil (the default) the
// scheduler is disabled entirely, so CLI/MCP modes and tests never start it.
func (a *Gateway) WithScheduledRunner(runner ScheduledRunner) *Gateway {
	a.runJob = runner
	return a
}

// WithJobConfirmer attaches the writer that persists a held job's approval back
// to the config store, so the scheduler does not re-ask after a restart. nil
// (the default) leaves approvals session-scoped. Returns the receiver for
// fluent wiring.
func (a *Gateway) WithJobConfirmer(confirm JobConfirmer) *Gateway {
	a.persistJobConfirmation = confirm
	return a
}

// WithJournalSweeper attaches the retention sweep and its cadence. The sweep
// runs once at startup and then every interval, inside the daemon (the single
// writer that may delete). nil disables it, so CLI/MCP and tests never sweep.
// Returns the receiver for fluent wiring.
func (a *Gateway) WithJournalSweeper(sweep func(context.Context) error, every time.Duration) *Gateway {
	a.journalSweep = sweep
	a.journalSweepEvery = every
	return a
}

// WithTroubleshootBundler attaches the diagnostics-bundle assembler that backs
// the `/murtaugh troubleshoot` slash command. nil (the default) disables the
// command. Returns the receiver for fluent wiring.
func (a *Gateway) WithTroubleshootBundler(bundler TroubleshootBundler) *Gateway {
	a.troubleshoot = bundler
	return a
}

func (a *Gateway) Run(ctx context.Context) error {
	// On shutdown/restart (ctx cancelled, superviseSocket returns) close every
	// agent's session manager so its backend processes — and the whole tree each
	// spawns (ACP adapter + mcp-bridge + claude + MCP servers) — are killed rather
	// than orphaned. Runs before the process exits (or before the restart
	// coordinator's os.Exit, within its grace window).
	defer a.closeChatSessions()

	resolveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := a.resolveAllowSet(resolveCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("resolve allowed users: %w", err)
	}
	// resolveAllowSet rewrites a.cfg with IDs; hand the resolved admin to the
	// auth flow, which was constructed with the raw (possibly handle) value and
	// would otherwise open a DM against "@someone".
	if a.auth != nil {
		a.auth.SetAdmin(a.cfg.AdminUser, a.cfg.IsAdminUser)
	}

	a.startBridge(ctx)
	a.logStartupRouting(ctx)
	a.warmChat(ctx)
	a.startChannelCache(ctx)
	a.startJournalSweeper(ctx)
	stopScheduler := a.startScheduler(ctx)
	defer stopScheduler()

	// The Slack socket is owned by a supervisor that reconnects on failure and
	// recycles a wedged (half-open) connection via a heartbeat watchdog, rather
	// than running socketmode.RunContext once and giving up when it returns.
	return a.superviseSocket(ctx)
}

// closeChatSessions closes every agent's session manager, tearing down its backend
// processes (and the process tree each spawned). Idempotent and safe to call when
// no session ever started; a manager that is not a Closer is skipped.
func (a *Gateway) closeChatSessions() {
	for name, mgr := range a.chatSessions {
		closer, ok := mgr.(io.Closer)
		if !ok {
			continue
		}
		if err := closer.Close(); err != nil {
			a.logger.Warn("failed to close agent sessions on shutdown", "agent", name, "error", err)
		}
	}
}

// startBridge binds the MCP aggregator socket and tears it down when ctx ends.
// A bind failure is logged and degrades to ACP agents having no Murtaugh tools,
// never blocking gateway startup.
func (a *Gateway) startBridge(ctx context.Context) {
	if a.bridge == nil {
		return
	}
	go func() {
		if err := a.bridge.Start(ctx); err != nil {
			a.logger.Warn("mcp aggregator disabled: could not start", "error", err)
		}
	}()
}

// bridgeSocketPath returns the per-process aggregator socket path. It lives under
// the temp dir (kept short — unix socket paths are length-capped) and carries the
// pid so concurrent gateways do not collide.
func bridgeSocketPath() string {
	return filepath.Join(os.TempDir(), "murtaugh", fmt.Sprintf("mcp-agg-%d.sock", os.Getpid()))
}

func (a *Gateway) warmChat(ctx context.Context) {
	if a.chat == nil {
		return
	}
	go func() {
		warmCtx, cancel := context.WithTimeout(ctx, a.chatWarmTimeout)
		defer cancel()
		if err := a.chat.Warm(warmCtx); err != nil {
			a.logger.Error("agent warmup failed", "error", err)
			return
		}
		a.logger.Info("agent warmup completed")
	}()
}

// startChannelCache launches the channel-name cache lifecycle in a goroutine
// that lives for the Run context. It warms the ID→name map once at startup (so
// the first channel messages can route by name) and refreshes it on a ticker.
// No-op when no cache is wired (no bot token, CLI/MCP, struct-literal test
// gateways), so only the Slack daemon path pays for it.
func (a *Gateway) startChannelCache(ctx context.Context) {
	if a.channelCache == nil {
		return
	}
	a.logger.Info("channel-name cache started", "every", defaultChannelCacheRefresh.String())
	go a.channelCache.run(ctx, defaultChannelCacheRefresh)
}

// startJournalSweeper launches the retention sweeper in a goroutine that lives
// for the Run context. It sweeps once at startup (the daemon is not always up,
// so a bare interval timer would drift across sleeps/restarts) and then every
// configured interval. No-op when no sweep is wired, so only the journal-enabled
// daemon path pays for it.
func (a *Gateway) startJournalSweeper(ctx context.Context) {
	if a.journalSweep == nil {
		return
	}
	every := a.journalSweepEvery
	if every <= 0 {
		every = 24 * time.Hour
	}
	a.logger.Info("journal sweeper started", "every", every.String())
	go func() {
		a.runJournalSweep(ctx)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.runJournalSweep(ctx)
			}
		}
	}()
}

// runJournalSweep performs one bounded sweep, logging (never propagating) any
// error so a transient failure does not stop the ticker.
func (a *Gateway) runJournalSweep(ctx context.Context) {
	sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := a.journalSweep(sweepCtx); err != nil {
		a.logger.Warn("journal sweep failed", "error", err)
	}
}

func (a *Gateway) handleEvent(ctx context.Context, event socketmode.Event) {
	switch event.Type {
	case socketmode.EventTypeConnected:
		a.logger.Info("Slack socket connected")
		a.recordConnection(ctx, journal.LevelInfo, "connected", "Slack socket connected", nil)
		a.notifyConnected(ctx)
	case socketmode.EventTypeConnecting, socketmode.EventTypeHello:
		a.logger.Debug("socket mode lifecycle event", "type", event.Type)
	case socketmode.EventTypeDisconnect:
		a.logger.Warn("Slack socket disconnected", "type", event.Type)
		a.recordConnection(ctx, journal.LevelWarn, "disconnected", "Slack socket disconnected", nil)
	case socketmode.EventTypeConnectionError, socketmode.EventTypeInvalidAuth, socketmode.EventTypeIncomingError:
		a.logger.Warn("Slack socket error", "type", event.Type)
		a.recordConnection(ctx, journal.LevelWarn, "error", fmt.Sprintf("Slack socket error: %s", event.Type), map[string]any{"event_type": string(event.Type)})
	case socketmode.EventTypeSlashCommand:
		a.handleSlashCommand(ctx, event)
	case socketmode.EventTypeInteractive:
		a.handleInteractive(event)
	case socketmode.EventTypeEventsAPI:
		a.handleEventsAPI(event)
	default:
		a.logger.Debug("ignored socket mode event", "type", event.Type)
	}
}

// resolveAllowSet resolves configuration.admin_user and configuration.allowed_users
// (each may be a Slack user ID or a handle) into IDs and rewrites a.cfg with
// the resolved values, so subsequent IsAllowedUser checks are ID-only. A
// single users.list call is made when any entry is a handle. Unresolvable
// entries are fatal (fail-closed). When both admin_user and allowed_users are
// empty the Gateway is effectively locked down and direct interactions will be
// denied; a warning is logged in that case.
func (a *Gateway) resolveAllowSet(ctx context.Context) error {
	// The admin and allowed_users entries plus the no-mention lists (global and
	// per-channel) may each be handles or IDs. They are resolved in one batched
	// users.list call by concatenating every reference, resolving once, and
	// slicing the result back into place — so a workspace with no handles makes
	// zero Slack calls and one with handles makes exactly one.
	hasAdmin := strings.TrimSpace(a.cfg.AdminUser) != ""

	// Deterministic per-channel key order so the resolved slices line up with the
	// keys when we split the result back out.
	channelKeys := make([]string, 0, len(a.noMentionPerChannel))
	for key := range a.noMentionPerChannel {
		channelKeys = append(channelKeys, key)
	}
	sort.Strings(channelKeys)

	refs := make([]string, 0, 1+len(a.cfg.AllowedUsers)+len(a.noMentionEverywhere))
	if hasAdmin {
		refs = append(refs, a.cfg.AdminUser)
	}
	refs = append(refs, a.cfg.AllowedUsers...)
	refs = append(refs, a.noMentionEverywhere...)
	for _, key := range channelKeys {
		refs = append(refs, a.noMentionPerChannel[key]...)
	}
	if len(refs) == 0 {
		a.logger.Warn("authorization locked down: configuration.admin_user and configuration.allowed_users are both empty; direct interactions will be ignored")
		return nil
	}
	ids, err := resolveUserIDs(ctx, a.api, refs)
	if err != nil {
		return err
	}

	// Slice the resolved IDs back into the same shapes, in the same order they
	// were appended above.
	cursor := 0
	if hasAdmin {
		a.cfg.AdminUser = ids[cursor]
		cursor++
	}
	a.cfg.AllowedUsers = ids[cursor : cursor+len(a.cfg.AllowedUsers)]
	cursor += len(a.cfg.AllowedUsers)
	a.noMentionEverywhere = ids[cursor : cursor+len(a.noMentionEverywhere)]
	cursor += len(a.noMentionEverywhere)
	if len(channelKeys) > 0 {
		resolvedPerChannel := make(map[string][]string, len(channelKeys))
		for _, key := range channelKeys {
			n := len(a.noMentionPerChannel[key])
			resolvedPerChannel[key] = ids[cursor : cursor+n]
			cursor += n
		}
		a.noMentionPerChannel = resolvedPerChannel
	}

	a.logger.Info("resolved authorized Slack users", "admin_user", a.cfg.AdminUser, "allowed_users", len(a.cfg.AllowedUsers))
	return nil
}

// notifyConnected runs the once-per-process, connect-time Slack greeting.
// Exactly one of two things happens, decided by whether a fresh restart marker
// is waiting on disk:
//
//   - Resuming from a restart: the "restarting…" notice is edited in place into
//     the back-online ping card, and the standalone startup ping is suppressed
//     (the operator already has a card to click — see point 2a of the redesign).
//   - Fresh boot / crash / no marker: the normal startup ping card is posted.
//
// Resolving the two against one loaded marker is what makes them mutually
// exclusive — a clean single greeting instead of three stacked messages. Slack
// may emit several Connected events per process (re-connects, flaky links);
// connectHandled guards against repeating the greeting.
func (a *Gateway) notifyConnected(ctx context.Context) {
	if a.connectHandled {
		return
	}
	a.connectHandled = true
	go func() {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		// A consumed marker renders the back-online ping card itself, so the
		// standalone startup ping would be redundant — skip it.
		if a.consumeResumeMarker(c) {
			return
		}
		if a.startupNotifier == nil {
			return
		}
		if err := a.startupNotifier.NotifyStartup(c); err != nil {
			a.logger.Error("startup Slack ping failed", "error", err)
		}
	}()
}

// interactionAdmission is how far a clicker's authority reaches over the
// interactive surface. It mirrors the chat surface: authority comes from the
// global allowlist, or — one step narrower — from the channel the click happened
// in.
type interactionAdmission int

const (
	// admissionDenied: the click is ignored.
	admissionDenied interactionAdmission = iota
	// admissionChannelGuest: the clicker is NOT on access.allowed_users, but the
	// channel's chat.channels rule sets allow_anyone, so they may talk to the
	// routed agent here. Their authority is limited to answering prompts the
	// agent itself raised in this conversation — `ask` questions and tool
	// approvals. It reaches nothing else: not the App Home controls, not restart,
	// not the workflow engine, whose actions are operator-configured and not
	// bounded by the channel agent's toolset.
	admissionChannelGuest
	// admissionAllowlisted: admin_user or access.allowed_users. The full
	// interactive surface, exactly as before allow_anyone existed.
	admissionAllowlisted
)

// handleInteractive gates an interactive callback by the SAME authority that
// governs the chat surface: admin_user / allowed_users, else the channel's
// allow_anyone rule. A user who may ask the agent to do something in a channel
// may also answer the questions that agent asks back — otherwise a guest's turn
// would stall on an approval prompt they are not allowed to click.
//
// Anything beyond the agent's own prompts stays allowlist-only, so opening a
// channel for chat still never widens who can restart the bot, install an
// update, or fire a configured workflow rule.
func (a *Gateway) handleInteractive(event socketmode.Event) {
	interaction, ok := event.Data.(slack.InteractionCallback)
	if !ok {
		a.ack(event)
		a.logger.Warn("unexpected interactive payload", "type", fmt.Sprintf("%T", event.Data))
		return
	}

	a.ack(event)
	// Fast path: an allowlisted clicker needs no channel context, so the common
	// case keeps running inline — this matters for the modal path below, whose
	// trigger_id expires in seconds.
	if a.cfg.IsAllowedUser(interaction.User.ID) {
		a.dispatchInteractive(event, interaction, admissionAllowlisted)
		return
	}
	// Otherwise the channel's rule decides, and that needs the channel NAME —
	// possibly a Slack round-trip, so it moves off the socket goroutine. In
	// practice the cache is already warm: a prompt only exists because a turn is
	// running in this channel, and that turn resolved the name to route itself.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), channelChatTimeout)
		channelName := a.resolveChannelNameFor(ctx, interaction.Channel.ID)
		cancel()
		if !channelAllowsAnyone(interaction.Channel.ID, channelName, a.chatChannels) {
			a.logger.Info("denied interactive callback from unauthorized user", "user", interaction.User.ID, "channel", interaction.Channel.ID, "callback_id", interaction.CallbackID)
			return
		}
		a.dispatchInteractive(event, interaction, admissionChannelGuest)
	}()
}

// dispatchInteractive routes an already-authorized callback. admission decides
// how much of the surface is reachable; see interactionAdmission.
func (a *Gateway) dispatchInteractive(event socketmode.Event, interaction slack.InteractionCallback, admission interactionAdmission) {
	if admission == admissionDenied {
		return
	}
	// The binary's own built-ins. All of them are allowlist-or-better and the
	// privileged ones (App Home update/restart, restart suggestion) additionally
	// re-check IsAdminUser in their handlers.
	if admission == admissionAllowlisted {
		a.dispatchInteractiveBuiltins(interaction)
		if a.builtinInteractionHandled(interaction) {
			return
		}
	}
	// Authentication cards. Routed before the broker because the namespaces are
	// distinct and this one is admin-only: the Flow re-checks IsAdminUser itself
	// rather than trusting the router, so a guest reaching this branch is
	// rejected there rather than here.
	if a.auth != nil {
		if corr, action, ok := authcard.IsAuthInteraction(interaction); ok {
			triggerID := interaction.TriggerID
			user := interaction.User.ID
			// The primary button may need to open a modal, and Slack expires a
			// trigger_id within seconds, so this runs promptly on its own
			// goroutine rather than behind the rest of the dispatch.
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := a.auth.HandleClick(ctx, corr, action, user, triggerID); err != nil {
					a.logger.Warn("auth card click not applied", "error", err, "correlation", corr, "action", string(action))
				}
			}()
			return
		}
		if interaction.Type == slack.InteractionTypeViewSubmission {
			if corr, code, ok := authcard.ParseCodeSubmission(interaction); ok {
				if err := a.auth.HandleCodeSubmission(corr, code, interaction.User.ID); err != nil {
					a.logger.Warn("auth code submission not applied", "error", err, "correlation", corr)
				}
				return
			}
		}
	}

	// Broker prompts — the `ask` tool AND the tool-approval gates (both the ACP
	// PermissionGate and the native approver post through this same broker). This
	// is the one surface a channel guest reaches: it is the agent asking a
	// question about the turn in front of them.
	if a.interactions != nil && askbroker.IsInteraction(interaction) {
		if corr, decision, ok := askbroker.ParseClick(interaction); ok {
			a.interactions.Resolve(corr, decision)
		}
		return
	}
	// The `ask` tool's multi-question card. One branch, because the card carries
	// its inputs inline: the click that presses Submit brings every input's state
	// with it, so there is no modal to open and no view_submission to wait for.
	if a.askCards != nil {
		// A radio pick or checkbox tick fires its own callback. It carries no
		// decision — the state rides along with the eventual Submit — so it is
		// swallowed here rather than falling through to the workflow engine.
		if askcard.IsCardInput(interaction) {
			return
		}
		if corr, action, ok := askcard.IsAskInteraction(interaction); ok {
			answers := askcard.ParseSubmission(interaction)
			userID := interaction.User.ID
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := a.askCards.HandleClick(ctx, corr, action, userID, answers); err != nil {
					a.logger.Error("handling ask card click failed", "error", err, "correlation", corr, "action", string(action))
				}
			}()
			return
		}
	}
	// Everything past here is the workflow engine: operator-configured rules that
	// can run commands and delegate to agents. Their blast radius is not bounded
	// by the channel agent's toolset, so a channel guest stops here.
	if admission != admissionAllowlisted {
		a.logger.Info("ignored non-prompt interactive callback from channel guest", "user", interaction.User.ID, "channel", interaction.Channel.ID, "callback_id", interaction.CallbackID)
		return
	}
	a.dispatchInteractiveWorkflow(event, interaction)
}

// builtinInteractionHandled reports whether interaction was claimed by one of
// the binary's built-in controls, so the router stops rather than handing it to
// the broker or the workflow engine.
func (a *Gateway) builtinInteractionHandled(interaction slack.InteractionCallback) bool {
	return isRestartSuggestionInteraction(interaction) ||
		isPingInteraction(interaction) ||
		isAppHomeUpdateClick(interaction) ||
		isAppHomeUpdateSubmit(interaction) ||
		isAppHomeRestartClick(interaction) ||
		isAppHomeRestartSubmit(interaction)
}

// dispatchInteractiveBuiltins runs the binary-owned controls. Each is handled
// before the workflow engine sees the callback so a configured rule or an
// on-disk template can never redirect them.
func (a *Gateway) dispatchInteractiveBuiltins(interaction slack.InteractionCallback) {
	if isRestartSuggestionInteraction(interaction) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			a.handleRestartSuggestionInteraction(ctx, interaction)
		}()
		return
	}
	// The built-in communication self-test ("Test communication" button) is
	// handled here, before the workflow engine, so the ping → pong round-trip is
	// owned entirely by the binary and cannot be redirected by a configured
	// workflow rule or an on-disk template.
	if isPingInteraction(interaction) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			a.handlePingInteraction(ctx, interaction)
		}()
		return
	}
	// App Home control panel: the admin's "Update" button opens a confirmation
	// modal; submitting it installs the release and restarts. Both are owned by
	// the binary (admin-gated) and handled before the workflow engine, like the
	// restart and ping built-ins above.
	if isAppHomeUpdateClick(interaction) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			a.handleAppHomeUpdateClick(ctx, interaction)
		}()
		return
	}
	if isAppHomeUpdateSubmit(interaction) {
		go a.handleAppHomeUpdateSubmit(interaction)
		return
	}
	// App Home "Restart Murtaugh" button: the admin's on-demand restart. Like
	// Update, the click opens a confirmation modal and the submit triggers the
	// coordinator; both are admin-gated and owned by the binary.
	if isAppHomeRestartClick(interaction) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			a.handleAppHomeRestartClick(ctx, interaction)
		}()
		return
	}
	if isAppHomeRestartSubmit(interaction) {
		go a.handleAppHomeRestartSubmit(interaction)
		return
	}
}

// dispatchInteractiveWorkflow hands the callback to the configured workflow
// rules. Allowlisted clickers only — see interactionAdmission.
func (a *Gateway) dispatchInteractiveWorkflow(event socketmode.Event, interaction slack.InteractionCallback) {
	// Mint a correlation id for this interaction and record its arrival. The
	// same id is propagated into the workflow engine via the context so the
	// match/no-match/trigger events all tie back to this one click.
	corrID := journal.NewCorrID("gw")
	a.record(journal.WithCorrID(context.Background(), corrID), "interactive.received", journal.LevelInfo,
		"interactive callback received",
		journal.Keys{TeamID: interaction.Team.ID, ChannelID: interaction.Channel.ID, UserID: interaction.User.ID},
		map[string]any{"interaction_type": string(interaction.Type), "callback_id": interaction.CallbackID})
	// The raw Slack callback bytes (as delivered) are what a `run` trigger gets
	// on stdin — full fidelity, exactly what Slack sent. Falls back to a
	// marshaled form inside the engine when absent.
	var rawPayload []byte
	if event.Request != nil {
		rawPayload = event.Request.Payload
	}
	go func() {
		// No total wall-clock deadline here: a reply-to-slack step backed by a
		// delegate (headless JSON) is legitimately long-running and is already
		// bounded by the delegate Runner's idle watchdog. The other trigger steps
		// are independently bounded too — a run command by its own commandTimeout,
		// a reply-to-slack POST by the Slack HTTP client — so a fixed cap here only
		// ever guillotines a productive long turn. A top-level delegate-to-agent
		// trigger starts a detached chat turn (its own lifecycle), so this
		// context's cancel() on Execute-return does not affect it.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ctx = journal.WithCorrID(ctx, corrID)
		if err := a.workflow.Execute(ctx, interaction, rawPayload); err != nil {
			a.logger.Error("interactive workflow failed", "error", err)
		}
	}()
}

func (a *Gateway) handleSlashCommand(ctx context.Context, event socketmode.Event) {
	command, ok := event.Data.(slack.SlashCommand)
	if !ok {
		a.ack(event, ephemeralText("Unsupported slash command payload."))
		a.logger.Warn("unexpected slash command payload", "type", fmt.Sprintf("%T", event.Data))
		return
	}

	if !a.cfg.IsAllowedUser(command.UserID) {
		a.logger.Info("denied slash command from unauthorized user", "command", command.Command, "user", command.UserID, "channel", command.ChannelID)
		a.ack(event, ephemeralText("Sorry, you are not authorized to use this command."))
		return
	}

	ctx = journal.WithCorrID(ctx, journal.NewCorrID("gw"))
	a.record(ctx, "slash.command", journal.LevelInfo, "slash command received",
		journal.Keys{TeamID: command.TeamID, ChannelID: command.ChannelID, UserID: command.UserID},
		map[string]any{"command": command.Command, "text": command.Text})

	response, err := a.handler.HandleSlashCommand(ctx, command)
	if isRestartSlashCommand(command.Text) {
		a.handleRestartSlashCommand(ctx, event, command)
		return
	}
	if isChatSlashCommand(command.Text) {
		a.handleChatSlashCommand(ctx, event, command)
		return
	}
	if isStopSlashCommand(command) {
		a.handleStopSlashCommand(event, command, slashCommandThreadTS(event))
		return
	}
	if isTroubleshootSlashCommand(command.Text) {
		a.handleTroubleshootSlashCommand(ctx, event, command)
		return
	}
	if err != nil {
		a.logger.Error("slash command failed", "command", command.Command, "error", err)
		response = ephemeralText("Murtaugh hit an error while handling that command.")
	}
	a.ack(event, response)
}

// handleRestartSlashCommand is invoked when an allowed user issues the
// `restart` verb. Authorization is two-layered: the outer
// handleSlashCommand has already checked IsAllowedUser, and this method
// additionally requires IsAdminUser. Non-admin allowed users receive an
// ephemeral deny so the failure mode is discoverable (unlike DMs or
// mentions, where silent ignore is the policy).
//
// On accept, the "restarting…" notice is posted to the originating
// channel and a resume marker is written to disk before the coordinator
// is signalled. The notice + marker are best-effort: any failure is
// logged but never blocks the restart itself (see resume.go).
func (a *Gateway) handleRestartSlashCommand(ctx context.Context, event socketmode.Event, command slack.SlashCommand) {
	if !a.cfg.IsAdminUser(command.UserID) {
		a.logger.Info("denied restart slash command from non-admin user", "command", command.Command, "user", command.UserID, "channel", command.ChannelID)
		a.ack(event, ephemeralText("Sorry, only the configured admin can restart Murtaugh."))
		return
	}
	if a.restart == nil {
		a.logger.Warn("restart slash command received but no coordinator is wired", "user", command.UserID, "channel", command.ChannelID)
		a.ack(event, ephemeralText("Restart is not available in this deployment."))
		return
	}
	reason := fmt.Sprintf("user requested via %s restart", command.Command)
	// Post + persist must happen before the coordinator fires so the
	// marker is durable when the grace timer expires and the process
	// exits. Use a fresh bounded context so a slow Slack API call does
	// not stall the slash ack.
	noticeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	a.postRestartNoticeAndSaveMarker(noticeCtx, command.ChannelID, "", command.UserID, restartSourceSlash, reason)
	cancel()
	if !a.restart(string(restartSourceSlash), command.UserID, command.ChannelID, reason) {
		a.ack(event, ephemeralText("A restart is already in progress (or the cool-down has not elapsed). Try again shortly."))
		return
	}
	a.ack(event, ephemeralText("Restarting Murtaugh now. I'll be back in a moment."))
}

func (a *Gateway) handleChatSlashCommand(ctx context.Context, event socketmode.Event, command slack.SlashCommand) {
	text := slashChatPrompt(command.Text)
	if text == "" {
		a.ack(event, ephemeralText("Usage: `/murtaugh chat <prompt>`"))
		return
	}
	if a.chat == nil {
		a.ack(event, ephemeralText("ACP chat is not enabled. Configure `agent.enabled: true` first."))
		return
	}
	a.ack(event, ephemeralText("Murtaugh is answering in the channel."))
	a.startChat(ctx, ChatRequest{TeamID: command.TeamID, ChannelID: command.ChannelID, UserID: command.UserID, Text: text, Source: "slash_command"})
}

// handleTroubleshootSlashCommand assembles a diagnostics bundle and uploads it
// to the admin's DM. Any allowed user may trigger it (the outer
// handleSlashCommand already enforced IsAllowedUser); the bundle is delivered
// only to the admin — never echoed to the invoking channel — because it can
// contain sensitive data. The bundle is built and uploaded in a goroutine so
// the slash command is acked within Slack's ~3s window.
func (a *Gateway) handleTroubleshootSlashCommand(ctx context.Context, event socketmode.Event, command slack.SlashCommand) {
	if a.troubleshoot == nil {
		a.ack(event, ephemeralText("Troubleshooting bundles are not available in this deployment."))
		return
	}
	admin := strings.TrimSpace(a.cfg.AdminUser)
	if admin == "" {
		a.ack(event, ephemeralText("No admin user is configured, so there is nowhere private to send the bundle."))
		return
	}
	if strings.TrimSpace(a.botToken) == "" {
		a.ack(event, ephemeralText("The bot token is not configured, so I cannot upload a bundle."))
		return
	}
	note := slashTroubleshootText(command.Text)
	a.ack(event, ephemeralText("Assembling a diagnostics bundle and sending it to the admin's DM. This can take a moment."))

	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		api, err := slackclient.NewClient(a.botToken)
		if err != nil {
			a.logger.Error("troubleshoot: build slack client failed", "error", err)
			return
		}
		dm, err := api.OpenDM(bgCtx, admin)
		if err != nil {
			a.logger.Error("troubleshoot: open admin DM failed", "error", err, "admin", admin)
			return
		}

		zipPath, warnings, err := a.troubleshoot(bgCtx, note)
		if err != nil {
			a.logger.Error("troubleshoot bundle failed", "error", err, "user", command.UserID)
			_, _ = api.PostMessage(bgCtx, slackclient.PostMessageParams{
				ChannelID: dm,
				Text:      fmt.Sprintf(":warning: Troubleshooting bundle requested by <@%s> failed: %v", command.UserID, err),
			})
			return
		}
		defer os.Remove(zipPath)

		// The narrative (header, symptom-or-not, redaction caveat) is a Block Kit
		// message; a file's initial_comment is text-only, so it must be its own post.
		// A failed post is logged but never blocks the upload — the zip is the point.
		if blocks, merr := json.Marshal(slack.Blocks{BlockSet: troubleshootBlocks(command.UserID, note, warnings)}); merr != nil {
			a.logger.Error("troubleshoot: marshal bundle message failed", "error", merr)
		} else if _, perr := api.PostMessage(bgCtx, slackclient.PostMessageParams{
			ChannelID: dm,
			Text:      troubleshootFallback(command.UserID, note),
			Blocks:    blocks,
		}); perr != nil {
			a.logger.Error("troubleshoot: post bundle message failed", "error", perr)
		}

		if _, err := api.UploadFile(bgCtx, slackclient.UploadFileParams{
			ChannelID: dm,
			FilePath:  zipPath,
			Filename:  filepath.Base(zipPath),
			Title:     "Murtaugh troubleshooting bundle",
		}); err != nil {
			a.logger.Error("troubleshoot: upload bundle failed", "error", err)
			_, _ = api.PostMessage(bgCtx, slackclient.PostMessageParams{
				ChannelID: dm,
				Text:      fmt.Sprintf(":warning: Assembled the diagnostics bundle but the upload failed: %v", err),
			})
			return
		}
		a.logger.Info("troubleshoot bundle delivered", "user", command.UserID, "warnings", len(warnings))
	}()
}

const (
	// troubleshootHeader titles the bundle message.
	troubleshootHeader = "Troubleshooting Bundle"
	// troubleshootNoContext is the middle block when the requester filed the
	// bundle with no symptom note — Murtaugh grumbling about working blind. The
	// %s is a mention of the requester so the admin knows who to ping.
	troubleshootNoContext = "Great! Nobody told me what's actually wrong with it. :unamused: Love workin' blind. Ping %s, they buzzed me about this."
	// troubleshootRedaction is the fixed redaction caveat, in Murtaugh's voice.
	troubleshootRedaction = "> :warning: I patted it down for the obvious stuff — Slack tokens, config secrets, the usual suspects. But the transcripts and them .db files? Nobody scrubbed those. Handle 'em like they're loaded."
)

// troubleshootBlocks builds the Block Kit message that accompanies an uploaded
// diagnostics bundle in the admin's DM. The header and redaction caveat are
// fixed; the middle block depends on whether the requester attached a symptom
// note. Non-fatal collection warnings, when present, are appended as their own
// block so nothing the plain-text comment used to carry is lost.
func troubleshootBlocks(userID, note string, warnings []string) []slack.Block {
	requester := fmt.Sprintf("<@%s>", userID)
	blocks := []slack.Block{
		slack.NewHeaderBlock(slack.NewTextBlockObject(slack.PlainTextType, troubleshootHeader, true, false)),
	}
	if strings.TrimSpace(note) == "" {
		blocks = append(blocks, mrkdwnSection(fmt.Sprintf(troubleshootNoContext, requester)))
	} else {
		blocks = append(blocks, mrkdwnSection(fmt.Sprintf("Here's what %s said about this:\n%s", requester, blockquote(strings.TrimSpace(note)))))
	}
	blocks = append(blocks, mrkdwnSection(troubleshootRedaction))
	if len(warnings) > 0 {
		var b strings.Builder
		b.WriteString("*Collection notes:*")
		for _, w := range warnings {
			fmt.Fprintf(&b, "\n• %s", w)
		}
		blocks = append(blocks, mrkdwnSection(b.String()))
	}
	return blocks
}

// troubleshootFallback is the plain-text notification/accessibility fallback for
// the Block Kit bundle message (Slack shows it in notifications and to clients
// that cannot render blocks).
func troubleshootFallback(userID, note string) string {
	if strings.TrimSpace(note) == "" {
		return fmt.Sprintf("Troubleshooting bundle requested by <@%s> (no symptom description).", userID)
	}
	return fmt.Sprintf("Troubleshooting bundle requested by <@%s>: %s", userID, strings.TrimSpace(note))
}

// mrkdwnSection wraps markdown text in a section block.
func mrkdwnSection(md string) slack.Block {
	return slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, md, false, false), nil, nil)
}

// blockquote prefixes every line of s with Slack's "> " quote marker, so a
// multi-line note is quoted whole rather than only its first line.
func blockquote(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

// handleStopSlashCommand cancels the in-flight chat for the
// conversation the command was invoked in. Slack's slash command
// payload carries `thread_ts` when the command was issued from inside
// a thread (the slack-go SlashCommand struct does not surface it, so
// the caller re-parses the raw socketmode payload via
// slashCommandThreadTS and passes it in). Sessions are bound to their
// thread (DMs included), so the key mirrors conversationKey: it carries
// the thread context and flags DM channels. A /stop must therefore be
// issued from inside the target thread to cancel it.
//
// Authorisation: the outer handleSlashCommand has already enforced
// IsAllowedUser, so no extra admin gate is required here.
func (a *Gateway) handleStopSlashCommand(event socketmode.Event, command slack.SlashCommand, threadTS string) {
	key := agent.ConversationKey{
		TeamID:    command.TeamID,
		ChannelID: command.ChannelID,
		ThreadTS:  threadTS,
		DM:        strings.HasPrefix(command.ChannelID, "D"),
	}
	// Drop any messages queued behind the current turn first, so /stop does not
	// leave a coalesced follow-up to fire after the user asked to stop.
	a.coalescer.clear(key)
	if a.inFlight.Cancel(key) {
		a.logger.Info("stop slash command cancelled in-flight chat", "user", command.UserID, "channel", command.ChannelID, "thread_ts", threadTS)
		a.ack(event, ephemeralText("Stopped."))
		return
	}
	a.ack(event, ephemeralText("Nothing to stop."))
}

func (a *Gateway) handleEventsAPI(event socketmode.Event) {
	eventsAPI, ok := event.Data.(slackevents.EventsAPIEvent)
	if !ok {
		a.ack(event)
		a.logger.Warn("unexpected Events API payload", "type", fmt.Sprintf("%T", event.Data))
		return
	}
	a.ack(event)
	switch inner := eventsAPI.InnerEvent.Data.(type) {
	case *slackevents.AppHomeOpenedEvent:
		a.handleAppHomeOpened(inner)
	case *slackevents.LinkSharedEvent:
		a.handleLinkShared(eventsAPI.TeamID, inner)
	case *slackevents.AppMentionEvent:
		if a.chat == nil {
			a.logger.Debug("ignored app_mention because chat is disabled")
			return
		}
		if inner.BotID != "" {
			return
		}
		a.handleAppMention(eventsAPI, inner)
	case *slackevents.MessageEvent:
		if a.chat == nil {
			a.logger.Debug("ignored message because chat is disabled")
			return
		}
		// Bot/self messages are never answered, in DMs or channels.
		if inner.BotID != "" {
			return
		}
		// Allow plain messages and file uploads ("file_share"); drop other
		// subtypes (edits, deletes, joins, bot messages, …). Without this a DM
		// that carries an attachment is silently ignored.
		if inner.SubType != "" && inner.SubType != "file_share" {
			return
		}
		if inner.ChannelType == "im" {
			a.handleDirectMessage(eventsAPI, inner, event)
			return
		}
		// A plain (non-mention) channel message: the bot only answers it when the
		// author is waived from the mention requirement for this channel. Users not
		// on the no-mention list still reach the bot via the app_mention path.
		a.handleChannelMessage(eventsAPI, inner, event)
	default:
		a.logger.Debug("ignored Events API event", "inner_type", eventsAPI.InnerEvent.Type)
	}
}

// handleDirectMessage answers a DM (channel_type "im"). It is the verbatim DM
// path lifted out of handleEventsAPI: authorize the author, drop redelivered
// duplicates, then start the chat. DMs never require a mention.
func (a *Gateway) handleDirectMessage(eventsAPI slackevents.EventsAPIEvent, inner *slackevents.MessageEvent, event socketmode.Event) {
	if !a.cfg.IsAllowedUser(inner.User) {
		a.logger.Debug("ignored DM from unauthorized user", "user", inner.User, "channel", inner.Channel)
		return
	}
	if a.isDuplicateEvent(eventsAPI.TeamID, inner.Channel, inner.TimeStamp) {
		a.logger.Info("ignored duplicate DM", "channel", inner.Channel, "ts", inner.TimeStamp)
		return
	}
	a.startChat(context.Background(), ChatRequest{TeamID: eventsAPI.TeamID, ChannelID: inner.Channel, UserID: inner.User, ThreadTS: inner.ThreadTimeStamp, MessageTS: inner.TimeStamp, Text: inner.Text, Files: eventFiles(event), DM: true, Source: "dm"})
}

// channelChatTimeout bounds the off-socket admission work for one channel
// message: at most one read-through conversations.info before the message is
// admitted or dropped.
const channelChatTimeout = 15 * time.Second

// resolveChannelNameFor returns the channel's name, resolving it read-through
// (cache → conversations.info → memoize) on a miss. It may do Slack I/O, so it
// MUST be called off the socket goroutine.
//
// An unresolvable name yields "", which only ever narrows what can match: a
// name-glob rule cannot fire, so a stranger is denied and routing falls back to
// the default agent. The failure mode is therefore closed, and — unlike the
// previous nameFor-only check — it is reached only on a genuine API failure
// rather than on every first message in a channel the periodic refresh has not
// listed yet.
func (a *Gateway) resolveChannelNameFor(ctx context.Context, channelID string) string {
	if a.channelCache == nil {
		return ""
	}
	name, _ := a.channelCache.resolveChannelName(ctx, channelID)
	return name
}

// mayChatInChannel reports whether userID may drive a chat turn in the given
// channel. A user on the global allowlist always may; otherwise the channel's
// winning chat.channels rule may waive the allowlist via allow_anyone.
//
// This waiver is scoped to the CHAT surface only. Slash commands, interactive
// callbacks (tool approvals) and DMs deliberately keep calling cfg.IsAllowedUser
// directly, so opening a channel for conversation never widens who can approve a
// tool call, pull a diagnostics bundle, or DM the bot.
func (a *Gateway) mayChatInChannel(userID, channelID, channelName string) bool {
	if a.cfg.IsAllowedUser(userID) {
		return true
	}
	return channelAllowsAnyone(channelID, channelName, a.chatChannels)
}

// handleAppMention answers an explicit @mention in a channel. Admission runs on
// a goroutine because authorizing a non-allowlisted author needs the channel
// NAME (to match an allow_anyone rule), and resolving a name the cache has not
// learned yet costs a Slack call that must not block the socket goroutine.
//
// Dedup stays AFTER authorization, as it did before: isDuplicateEvent is a
// check-and-set under one mutex, so of the twin app_mention/plain-message
// deliveries that share a ts exactly one wins — but only among the deliveries
// that actually passed their checks. Consuming the slot earlier would let a
// plain-message twin that is about to fail the no-mention check swallow the
// mention that would have been answered.
func (a *Gateway) handleAppMention(eventsAPI slackevents.EventsAPIEvent, inner *slackevents.AppMentionEvent) {
	teamID, channelID, user, ts := eventsAPI.TeamID, inner.Channel, inner.User, inner.TimeStamp
	threadTS, text, files := inner.ThreadTimeStamp, inner.Text, inner.Files
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), channelChatTimeout)
		channelName := a.resolveChannelNameFor(ctx, channelID)
		cancel()
		if !a.mayChatInChannel(user, channelID, channelName) {
			a.logger.Debug("ignored app_mention from unauthorized user", "user", user, "channel", channelID)
			return
		}
		if a.isDuplicateEvent(teamID, channelID, ts) {
			a.logger.Info("ignored duplicate app_mention", "channel", channelID, "ts", ts)
			return
		}
		a.startChat(context.Background(), ChatRequest{TeamID: teamID, ChannelID: channelID, UserID: user, ThreadTS: threadTS, MessageTS: ts, Text: stripSlackMentions(text), Files: files, Source: "app_mention"})
	}()
}

// handleChannelMessage answers a plain (non-mention) message posted in a public
// channel or private group. The bot normally only replies to explicit
// @mentions there; this path waives the mention requirement for authors listed
// in the effective no-mention set — the UNION of chat.no_mention.everywhere and
// the chat.no_mention.by_channel entries whose glob matches this channel.
//
// The two gates are independent and both must pass: mayChatInChannel decides
// WHETHER the author may talk to the bot here (global allowlist, or the
// channel's allow_anyone rule), and the no-mention set decides whether they may
// do so WITHOUT an @mention. allow_anyone deliberately does not imply a mention
// waiver — otherwise an opened channel would have the bot answering every
// message posted in it.
//
// Admission runs on a goroutine so the channel name can be resolved
// read-through; both gates then judge the message against that one resolved
// name rather than one of them silently seeing "". See handleAppMention for why
// dedup comes last.
func (a *Gateway) handleChannelMessage(eventsAPI slackevents.EventsAPIEvent, inner *slackevents.MessageEvent, event socketmode.Event) {
	teamID, channelID, user, ts := eventsAPI.TeamID, inner.Channel, inner.User, inner.TimeStamp
	threadTS, text, files := inner.ThreadTimeStamp, inner.Text, eventFiles(event)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), channelChatTimeout)
		channelName := a.resolveChannelNameFor(ctx, channelID)
		cancel()
		if !a.mayChatInChannel(user, channelID, channelName) {
			a.logger.Debug("ignored channel message from unauthorized user", "user", user, "channel", channelID)
			return
		}
		allowed := usersAllowedWithoutMention(channelID, channelName, a.noMentionEverywhere, a.noMentionPerChannel)
		if !allowed[user] {
			a.logger.Debug("ignored channel message: author not waived from mention requirement", "user", user, "channel", channelID)
			return
		}
		if a.isDuplicateEvent(teamID, channelID, ts) {
			a.logger.Info("ignored duplicate channel message", "channel", channelID, "ts", ts)
			return
		}
		// Strip any mentions so the prompt is clean whether or not the user @mentioned
		// the bot (a listed user who also mentions is de-duped above via the shared ts).
		a.startChat(context.Background(), ChatRequest{TeamID: teamID, ChannelID: channelID, UserID: user, ThreadTS: threadTS, MessageTS: ts, Text: stripSlackMentions(text), Files: files, Source: "channel_no_mention"})
	}()
}

func (a *Gateway) handleLinkShared(teamID string, inner *slackevents.LinkSharedEvent) {
	if a.unfurl == nil {
		a.logger.Debug("ignored link_shared because no unfurl-rules are configured")
		return
	}
	req := LinkSharedRequest{
		TeamID:    teamID,
		ChannelID: inner.Channel,
		UserID:    inner.User,
		MessageTS: inner.MessageTimeStamp,
		ThreadTS:  inner.ThreadTimeStamp,
		Links:     inner.Links,
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), a.unfurlTimeout)
		defer cancel()
		ctx = journal.WithCorrID(ctx, journal.NewCorrID("gw"))
		if err := a.unfurl.Handle(ctx, req); err != nil {
			a.logger.Error("link unfurl failed", "channel", inner.Channel, "error", err)
		}
	}()
}

// isDuplicateEvent reports whether a message event (identified by its
// immutable Slack timestamp) has already been handled. Slack may deliver the
// same event more than once; without this guard a redelivery spawns a second
// chat that interrupts the first. An empty timestamp or unconfigured dedup
// cache is never treated as a duplicate.
func (a *Gateway) isDuplicateEvent(teamID, channelID, ts string) bool {
	if ts == "" {
		return false
	}
	return a.recentEvents.seenBefore(teamID + "|" + channelID + "|" + ts)
}

// agentInterruptible reports whether the resolved agent supports interrupting
// an in-flight prompt. Unknown agents, and session managers that do not expose
// the capability, are treated as interruptible so behaviour is unchanged unless
// detection explicitly says otherwise.
func (a *Gateway) agentInterruptible(agent string) bool {
	sessions, ok := a.chatSessions[agent]
	if !ok {
		return true
	}
	checker, ok := sessions.(interface{ Interruptible() bool })
	if !ok {
		return true
	}
	return checker.Interruptible()
}

// startChat resolves the route and submits the message to the coalescer, which
// batches rapid-fire and mid-turn follow-ups into a single coalesced turn per
// conversation and calls back into dispatchTurn when a turn should start. It
// replaces the older interrupt-and-replace / drop-the-follow-up behaviour. The
// grace window (in dispatchTurn's cancel closure) still lets trailing chunks
// already on the wire flush as "_interrupted_" rather than vanish, which is what
// ChatHandler.Handle renders when it sees context.Canceled (vs DeadlineExceeded).
func (a *Gateway) startChat(parent context.Context, req ChatRequest) {
	// Resolve the route once so session binding and dispatch agree on the agent
	// and reply strategy. A nil resolver (chat disabled / some tests) defaults to
	// threaded, matching the historical behaviour.
	route := ChatRoute{ReplyOnThread: true}
	if a.chat != nil && a.chat.resolver != nil {
		route = a.chat.resolver(req)
	}
	key := conversationKey(req, route.ReplyOnThread)
	// Hand the message to the coalescer instead of dispatching (or interrupting)
	// directly: it batches rapid-fire and mid-turn follow-ups into a single
	// coalesced turn per conversation. It calls back into dispatchTurn when a
	// turn should actually start. No message is dropped.
	if a.coalescer == nil {
		// No coalescer wired (a Gateway built outside New, e.g. in unit tests):
		// dispatch immediately, preserving one-message-one-turn behaviour.
		a.dispatchTurn(parent, key, route.Agent, route, req)
		return
	}
	a.coalescer.submit(parent, key, route.Agent, route, req)
}

// dispatchTurn launches one chat goroutine for an (already-coalesced) request
// and wires it into the in-flight registry so /stop can cancel it and the
// coalescer can interrupt it for the next batch. On completion it notifies the
// coalescer, which drains any messages that queued during the turn.
//
// The cancellation closure stored in the registry runs a two-step
// graceful-then-hard sequence: ask the agent to cancel its prompt (best-effort,
// non-blocking) and, after cancelGrace, hard-cancel the goroutine's context. The
// grace window lets trailing chunks already on the wire flush as "_interrupted_"
// rather than vanish.
func (a *Gateway) dispatchTurn(parent context.Context, key agent.ConversationKey, agentName string, route ChatRoute, req ChatRequest) {
	// Authoritative agent resolution happens here, off the socket goroutine
	// (dispatchTurn is invoked from the coalescer's timer/worker goroutines, not
	// the event loop). startChat resolved the route cache-only for the coalescer
	// key; now synchronously resolve the channel (one bounded conversations.info
	// on a miss) so a canvas turn — or any cold-cache channel — routes by its real
	// channel instead of the default agent. Only the agent is corrected; the reply
	// strategy from startChat is kept so the conversation key never diverges.
	if a.chat != nil && a.chat.resolver != nil {
		a.channelCache.resolveChannelName(parent, req.ChannelID)
		route.Agent = a.chat.resolver(req).Agent
		agentName = route.Agent
	}

	// No total wall-clock deadline: a turn is bounded by inactivity inside
	// ChatHandler (WithIdleTimeout), so a long-but-progressing response is never
	// killed mid-flight. This context stays cancellable purely for the interrupt
	// and /stop paths.
	ctx, cancelCtx := context.WithCancel(parent)
	cancelFunc := a.buildInterruptCancel(key, agentName, cancelCtx)
	if _, previous := a.inFlight.Register(key, cancelFunc, agentName); previous != nil {
		// The coalescer serialises dispatch per conversation, so a live previous
		// entry is unexpected; cancel it defensively rather than leak its
		// goroutine.
		a.logger.Warn("unexpected in-flight chat at dispatch; cancelling it", "channel", req.ChannelID, "thread_ts", key.ThreadTS, "agent", agentName)
		previous()
	}
	go func() {
		defer cancelCtx()
		err := a.chat.Handle(ctx, req, route)
		a.inFlight.Cancel(key) // self-unregister; no-op if /stop already removed it
		if a.coalescer != nil {
			a.coalescer.onComplete(key) // drain any messages queued during this turn
		}
		if err != nil {
			a.logger.Error("agent chat failed", "source", req.Source, "channel", req.ChannelID, "error", err)
			// Journal it too. A failed turn is exactly what someone debugging
			// "Murtaugh said it replied but nothing arrived" comes looking for, and
			// stderr is the one place they cannot filter. Recorded on a fresh
			// context carrying the turn's correlation id, because ctx is cancelled
			// the moment this goroutine returns.
			a.record(journal.WithCorrID(context.Background(), journal.CorrIDFromContext(ctx)),
				"chat.failed", journal.LevelError,
				"agent chat failed: "+truncateForSummary(err.Error(), 160),
				journal.Keys{TeamID: req.TeamID, ChannelID: req.ChannelID, ThreadTS: key.ThreadTS, UserID: req.UserID},
				map[string]any{"source": req.Source, "agent": agentName, "error": err.Error()})
		}
	}()
}

// buildInterruptCancel returns the cancellation closure stored in the
// in-flight registry for one chat goroutine. It is invoked either by
// the next message on the same conversation (interrupt path) or by the
// /stop slash command. The sequence:
//
//  1. Look up the live ACP session ID for the conversation. If there is
//     one, fire a non-blocking session/cancel — it tells the agent to
//     stop generating but keeps the session alive for the follow-up.
//  2. Wait cancelGrace, then hard-cancel the chat goroutine's context.
//     The grace timer runs in its own goroutine so the registry call
//     returns immediately; the chat goroutine itself will see the
//     context cancellation and unwind through ChatHandler.Handle's
//     interrupted path.
//
// Resolution of agent name → session manager uses chatSessions, which
// the Gateway captured at construction time. When ACP is disabled the
// closure degenerates to a plain cancelCtx call.
func (a *Gateway) buildInterruptCancel(key agent.ConversationKey, agent string, cancelCtx context.CancelFunc) context.CancelFunc {
	return func() {
		go func() {
			if a.chatSessions != nil {
				if sessions, ok := a.chatSessions[agent]; ok {
					if sessionID, live := sessions.Lookup(key); live {
						cancelReqCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						if err := sessions.Cancel(cancelReqCtx, sessionID); err != nil {
							a.logger.Warn("agent session cancel failed", "agent", agent, "session_id", sessionID, "error", err)
						}
						cancel()
					}
				}
			}
			time.Sleep(a.cancelGrace)
			cancelCtx()
		}()
	}
}

func isChatSlashCommand(text string) bool {
	fields := strings.Fields(text)
	return len(fields) > 0 && strings.EqualFold(fields[0], "chat")
}

func isTroubleshootSlashCommand(text string) bool {
	fields := strings.Fields(text)
	return len(fields) > 0 && strings.EqualFold(fields[0], "troubleshoot")
}

// slashTroubleshootText returns the free-text symptom description following the
// `troubleshoot` verb, or "" when the verb is absent. An empty description is
// allowed (the bundle is still useful) — it is not an error.
func slashTroubleshootText(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "troubleshoot") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), fields[0]))
}

// isStopSlashCommand recognises both the standalone `/stop` slash
// command (carried in command.Command) and the `<command> stop` verb
// form (carried in command.Text), so operators can wire either shape
// in the Slack app config. Matching is case-insensitive and tolerant
// of leading/trailing whitespace.
func isStopSlashCommand(command slack.SlashCommand) bool {
	if strings.EqualFold(strings.TrimSpace(command.Command), "/stop") {
		return true
	}
	fields := strings.Fields(command.Text)
	return len(fields) > 0 && strings.EqualFold(fields[0], "stop")
}

// slashCommandThreadTS extracts thread_ts from the raw socketmode
// payload. Slack includes the field on slash command invocations made
// from inside a thread, but slack-go's SlashCommand struct does not
// surface it, so we re-parse the JSON. Returns "" when the field is
// absent (channel-root invocations and DMs), which the caller treats
// as a channel-scoped lookup.
func slashCommandThreadTS(event socketmode.Event) string {
	if event.Request == nil {
		return ""
	}
	var payload struct {
		ThreadTS string `json:"thread_ts"`
	}
	if err := json.Unmarshal(event.Request.Payload, &payload); err != nil {
		return ""
	}
	return payload.ThreadTS
}

// eventFiles extracts the file attachments from the raw Events API payload.
// slack-go's MessageEvent struct does not surface `files` (only AppMentionEvent
// does), so we re-parse the JSON — the same approach as slashCommandThreadTS.
// Returns nil when there is no payload or no files.
func eventFiles(event socketmode.Event) []slack.File {
	if event.Request == nil {
		return nil
	}
	var payload struct {
		Event struct {
			Files []slack.File `json:"files"`
		} `json:"event"`
	}
	if err := json.Unmarshal(event.Request.Payload, &payload); err != nil {
		return nil
	}
	return payload.Event.Files
}

// restartSourceSlash mirrors internal/app.RestartSourceSlash. It is
// duplicated here to keep gateway independent of the composition root
// (importing internal/app would cycle back). Compatibility is enforced by
// keeping both string values identical.
const restartSourceSlash = "slash"

func isRestartSlashCommand(text string) bool {
	fields := strings.Fields(text)
	return len(fields) > 0 && strings.EqualFold(fields[0], "restart")
}

func slashChatPrompt(text string) string {
	fields := strings.Fields(text)
	if len(fields) <= 1 || !strings.EqualFold(fields[0], "chat") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), fields[0]))
}

func stripSlackMentions(text string) string {
	fields := strings.Fields(text)
	kept := fields[:0]
	for _, field := range fields {
		if strings.HasPrefix(field, "<@") && strings.HasSuffix(field, ">") {
			continue
		}
		kept = append(kept, field)
	}
	return strings.Join(kept, " ")
}

func (a *Gateway) ack(event socketmode.Event, response ...any) {
	if event.Request == nil {
		a.logger.Warn("cannot acknowledge event without request", "type", event.Type)
		return
	}
	socket := a.currentSocket()
	if socket == nil {
		return // no-op when no socket is wired (struct-literal test gateways)
	}
	if err := socket.Ack(*event.Request, response...); err != nil {
		a.logger.Error("failed to acknowledge Slack request", "error", err)
	}
}
