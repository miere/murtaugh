package agent

import (
	"context"
	"errors"
)

// ErrNonJSONOutput is returned by a delegation runner's RunForJSON when the
// agent completed its turn but its output was not a valid JSON document. The
// runner logs a warning with the raw output before returning it, so callers
// should simply skip rendering.
//
// It lives here rather than beside the runner because the runner reaches its
// callers through small local interfaces (workflow.AgentDelegator,
// gateway.UnfurlDelegator) and this sentinel is part of that contract. Keeping
// it here is what lets a caller branch on it without importing the runner —
// which, for the Slack gateway, would mean reaching an agent backend three hops
// down (#170 Change E).
var ErrNonJSONOutput = errors.New("delegate-to-agent: agent output was not valid JSON")

// ErrSessionGone means the session id a Prompt named no longer resolves to
// anything that could serve it, and that opening a fresh session would.
//
// It exists because of the split. In process a session id is only ever invalid
// because the agent died, and the manager has nothing better to do than report
// it. With a broker in front of several runtime nodes, a session id is minted BY
// a node, so a node disconnecting invalidates every id it minted while the
// conversation itself is perfectly servable — by somebody else. SessionManager
// answers this one error by discarding the binding and opening a new session,
// once. Every other error is still the caller's to render.
//
// It is a sentinel rather than a string match for the reason #170 gives about
// error identity crossing a wire: text survives serialisation and identity does
// not, so anything that has to be compared by identity needs a name.
var ErrSessionGone = errors.New("the runtime node holding this session is no longer connected")

type Client interface {
	Initialize(context.Context) error
	NewSession(context.Context, SessionMetadata) (Session, error)
	Prompt(context.Context, string, PromptRequest) (<-chan Event, error)
	Cancel(context.Context, string) error
	Close() error
}

type Session struct {
	ID string
}

type SessionMetadata struct {
	TeamID    string `json:"teamId,omitempty"`
	ChannelID string `json:"channelId,omitempty"`
	// ChannelName is the channel's Slack name without the leading '#', when the
	// gateway had resolved it. Empty for a DM, and empty for a channel whose
	// name the gateway's cache had not learned yet.
	//
	// It is carried because delegation matches a node's channel claims, and a
	// claim is an exact channel id, an exact channel NAME, or a glob over the
	// name — so an id alone can only ever match the first of the three, and the
	// worked example in #170 (`nc-*`, `review-*`) is entirely the other two.
	ChannelName string `json:"channelName,omitempty"`
	ThreadTS    string `json:"threadTs,omitempty"`
	UserID      string `json:"userId,omitempty"`
	Source      string `json:"source,omitempty"`
	// Surface names the Slack surface the turn originates from when it is not an
	// ordinary channel/DM — currently "canvas" for a canvas comment thread. Empty
	// means an ordinary surface. Set by the gateway's cold-session discovery.
	Surface string `json:"surface,omitempty"`
	// CanvasID is the canvas file id (F…) when Surface == "canvas", so tools can
	// read or edit the document the bot was mentioned in. Empty otherwise.
	CanvasID string `json:"canvasId,omitempty"`
	// Ephemeral marks a session that belongs to no conversation: a one-shot
	// delegation (job, workflow trigger, unfurl) rather than a Slack thread.
	// There is nothing to resume and nothing to route back, so DeriveSessionID
	// gives it a fresh random id instead of a derived one. Without this the
	// whole conversation triple is empty, every delegation hashes to the same
	// id, and a claude_code backend `--resume`s the previous delegation's
	// transcript — see DeriveSessionID.
	Ephemeral bool `json:"ephemeral,omitempty"`
}

// Aggregator hands an ACP session the MCP server it should connect to in order
// to reach Murtaugh's own tools (built-ins plus proxied external MCP). The
// concrete implementation lives above this package (it needs the tool registry,
// the MCP client, and the bridge); ProcessClient only knows this seam, so the
// agent package stays free of those dependencies. nil means the agent is told
// about no Murtaugh MCP server (the prior behaviour).
type Aggregator interface {
	// RegisterSession binds a session (its Slack location drives where approval
	// prompts are asked) and returns the MCP server spec to advertise in
	// session/new, plus a release to call when the session ends. An error means
	// no server is advertised; the agent simply gets no Murtaugh tools.
	RegisterSession(meta SessionMetadata) (server MCPServerSpec, release func(), err error)
}

// MCPServerSpec is the stdio MCP server an ACP agent is asked to spawn — the
// `murtaugh mcp-bridge` proxy. It maps onto ACP's stdio McpServer shape in
// session/new (name + command + args + env).
type MCPServerSpec struct {
	Name    string
	Command string
	Args    []string
	// Env are the KEY=VALUE pairs the agent sets on the spawned bridge. Rendered
	// to ACP's [{name,value}] env shape in stable key order.
	Env map[string]string
}

type PromptRequest struct {
	Text string `json:"text"`

	// Channel and Thread carry the Slack conversation the prompt originates
	// from. ACP has no system/instructions field, so when these are set the
	// ProcessClient prepends a delimited context block to the prompt — the
	// closest equivalent — telling the agent where it is. The agent passes
	// them on to the `restart` tool so the approval card is asked in this
	// conversation rather than the admin DM. Empty for non-chat callers.
	Channel string `json:"channel,omitempty"`
	Thread  string `json:"thread,omitempty"`

	// User is the Slack user who sent the message driving this turn. It rides
	// alongside Channel/Thread so the native loop can scope an approval prompt
	// to that one person (an ephemeral message) instead of the whole thread.
	// Empty for non-chat callers.
	User string `json:"user,omitempty"`

	// History carries a pre-rendered transcript of the Slack thread that
	// preceded this prompt. ACP's session/prompt is a single user turn with no
	// way to replay prior turns, so when a brand-new session is opened for an
	// existing thread the gateway flattens the backstory into this opaque text
	// block (already framed and author-labelled) and the ProcessClient emits it
	// as its own content block ahead of the user's message. Empty when the
	// session is warm (the agent already holds the history) or the thread is
	// new.
	History string `json:"history,omitempty"`
}

type Event struct {
	Type EventType
	Text string
	// StopReason is the agent's reported reason for ending a turn, carried on
	// EventComplete (e.g. "end_turn", "max_tokens", "refusal"). Empty when the
	// agent did not report one. The chat handler surfaces a non-"end_turn"
	// reason to the user so a turn that ends without a reply is not silent.
	StopReason string
	Error      error
	Task       *TaskEvent
	// Attachment carries a file the agent wants delivered to the user as a
	// first-class part of its reply (EventAttachment), independent of the text
	// stream. Both backends emit it: the native loop when a tool yields a file,
	// the ACP client when an agent message carries a binary content block. The
	// chat handler uploads it into the turn's thread.
	Attachment *AttachmentEvent
	// Permission carries an agent-initiated permission request (EventPermission)
	// that must be answered by a human before the agent proceeds. The ACP client
	// emits it for a session/request_permission so the request rides the SAME
	// ordered event stream as the reply text and task updates — letting the chat
	// handler settle any open reply and post the approval card in order, the
	// coordination the native loop gets for free by gating a tool call inline.
	// nil except on EventPermission.
	Permission *PermissionPrompt
}

type EventType string

const (
	EventText       EventType = "text"
	EventStatus     EventType = "status"
	EventComplete   EventType = "complete"
	EventError      EventType = "error"
	EventTask       EventType = "task"
	EventAttachment EventType = "attachment"
	EventPermission EventType = "permission"
)

// AttachmentEvent is a file the agent is sending to the user as part of its
// reply. It is the backend-neutral carrier consumed by the Slack chat handler,
// which uploads it into the turn's thread. The bytes come from exactly one of
// two sources: Path (a file on the daemon host, produced by an in-process
// native tool) or Data (in-memory bytes decoded from an ACP content block).
// When both are set Data wins; when neither is set the attachment is dropped.
type AttachmentEvent struct {
	// Filename is the suggested download name shown to the user. When empty the
	// handler derives one from the path or mimetype.
	Filename string
	// Title is an optional display title; defaults to the filename when empty.
	Title string
	// Comment is an optional message posted alongside the file (the caption).
	Comment string
	// Mimetype is the optional content type, used to derive a filename extension
	// when Filename is empty.
	Mimetype string
	// Path is a file on the daemon host to upload. Used by native tools.
	Path string
	// Data is the in-memory file content to upload. Used by ACP attachments.
	Data []byte
}

type TaskStatus string

const (
	TaskStatusPending    TaskStatus = "pending"
	TaskStatusInProgress TaskStatus = "in_progress"
	TaskStatusComplete   TaskStatus = "complete"
	TaskStatusFailed     TaskStatus = "failed"
	TaskStatusCancelled  TaskStatus = "cancelled"
)

// TaskKind distinguishes a real tool invocation from an entry in the agent's
// plan (its structured to-do list). The Slack renderer uses it to decide
// sealing: a new tool run ends the open reply-text message and opens a tool
// block, but a plan update never chops the reply. An ACP `plan` is re-sent as a
// full snapshot many times per turn, so treating its entries as tool runs would
// shred the streaming prose (the mid-word/confetti split this guards against).
// The zero value is a tool, so every existing producer — native tool events and
// ACP tool_call/tool_call_update — stays a tool with no change.
type TaskKind string

const (
	TaskKindTool TaskKind = ""     // a tool invocation (default)
	TaskKindPlan TaskKind = "plan" // an entry in the agent's plan snapshot
)

type TaskEvent struct {
	ID          string
	Title       string
	Status      TaskStatus
	Description string
	Output      string
	// Kind separates a tool invocation (default) from a plan-snapshot entry; see
	// TaskKind. Only the ACP plan extractor sets it to TaskKindPlan.
	Kind TaskKind
}

type ConversationKey struct {
	TeamID    string
	ChannelID string
	ThreadTS  string
	DM        bool
}

// TurnLocation is the Slack conversation a turn is running in. The native client
// stashes it on the per-turn context so interactive tools (e.g. `ask`) can post
// their prompt into the same thread — reliably, without depending on the model to
// pass the channel/thread as arguments.
//
// UserID is the person who triggered this turn. The approval gate uses it to post
// its confirmation prompt ephemerally — visible only to that user — rather than to
// the whole conversation. Empty for non-chat callers.
type TurnLocation struct {
	ChannelID string
	ThreadTS  string
	UserID    string
}

type conversationKeyCtx struct{}

// WithConversation returns ctx carrying the conversation key a turn belongs to.
//
// SessionManager puts it there on every Prompt, because the manager is the only
// thing that holds both the key and the client: the key never crosses
// agent.Client (NewSession takes metadata, Prompt takes a session id), and
// SessionMetadata cannot stand in for it — it carries no DM flag, so the two
// surfaces a conversation key deliberately keeps apart would collapse.
//
// It exists for delegation (#196): the gateway's broker has to know WHICH
// conversation it is electing a runtime node for, and it is reached at
// agent.Client. Every in-process backend ignores it, exactly as they ignore
// TurnLocation on the paths that do not carry one.
func WithConversation(ctx context.Context, key ConversationKey) context.Context {
	return context.WithValue(ctx, conversationKeyCtx{}, key)
}

// ConversationFromContext returns the conversation key stashed on ctx.
//
// ok is false for a caller with no conversation at all — a job, an unfurl, a
// workflow trigger — which is a real state and not a failure: those are
// #170's item 13, and a client that cannot identify a conversation must not
// invent one to pin.
func ConversationFromContext(ctx context.Context) (ConversationKey, bool) {
	key, ok := ctx.Value(conversationKeyCtx{}).(ConversationKey)
	return key, ok && key.ChannelID != ""
}

type turnLocationKey struct{}

// WithTurnLocation returns ctx carrying loc.
func WithTurnLocation(ctx context.Context, loc TurnLocation) context.Context {
	return context.WithValue(ctx, turnLocationKey{}, loc)
}

// TurnLocationFromContext returns the Slack location stashed on ctx, if any. ok
// is false when the turn carries no usable location (e.g. CLI/MCP callers), so
// interactive tools can refuse to run rather than block forever.
func TurnLocationFromContext(ctx context.Context) (TurnLocation, bool) {
	loc, ok := ctx.Value(turnLocationKey{}).(TurnLocation)
	return loc, ok && loc.ChannelID != ""
}

type turnEnvKey struct{}

// WithTurnEnv returns ctx carrying the calling agent's environment overrides as
// KEY=VALUE pairs.
//
// It exists because a tool that spawns a process on an agent's behalf is not the
// agent: it runs inside the daemon, so it inherits the DAEMON's environment. For
// most tools that is harmless, but `auth.request` shells out to the very CLI the
// agent is about to use — and if the agent's profile redirects that CLI's state
// (CLOUDSDK_CONFIG, AWS_CONFIG_FILE, …) while the sign-in does not, the
// credentials land in one place and are read from another. The symptom is an
// authentication that reports success and changes nothing.
//
// Empty for a native agent, which has no process environment of its own, and for
// CLI/MCP callers, which are not acting for any agent.
func WithTurnEnv(ctx context.Context, env []string) context.Context {
	if len(env) == 0 {
		return ctx
	}
	return context.WithValue(ctx, turnEnvKey{}, append([]string(nil), env...))
}

// TurnEnvFromContext returns the calling agent's environment overrides, or nil.
func TurnEnvFromContext(ctx context.Context) []string {
	env, _ := ctx.Value(turnEnvKey{}).([]string)
	return env
}

// PermissionOption is one choice an ACP agent offers for a session/request_permission
// request. Kind is the ACP PermissionOptionKind ("allow_once", "allow_always",
// "reject_once", "reject_always"); ID is the optionId echoed back in the response.
type PermissionOption struct {
	ID   string
	Name string
	Kind string
}

// PermissionRequest is an agent-initiated session/request_permission: the agent is
// about to use a tool and wants the client to pick one of Options.
//
// ToolKind is the ACP toolCall.kind (e.g. "execute", "edit", "read") — a stable,
// concise identifier used for the short tool label shown to the human. ToolTitle
// is the agent's human-readable title for the call (the ACP toolCall.title); for
// an execute call this is the command line, which the approval prompt renders as a
// fenced code block.
type PermissionRequest struct {
	SessionID string
	ToolKind  string
	ToolTitle string
	Options   []PermissionOption

	// PolicyOwned says the backend has no options of its own and is delegating the
	// whole decision to Murtaugh, which then supplies its own set (approve /
	// approve & always allow / deny) and answers with one of the Permission*
	// constants below.
	//
	// It exists because this struct is ACP-shaped, and only ACP actually carries
	// options on the wire. Claude Code's can_use_tool is a bare allow/deny with no
	// option list, so its backend used to invent a two-item one purely to satisfy
	// the shape — a constant dressed as protocol, which is why Murtaugh's own
	// always-allow could never appear there. Setting this instead keeps the
	// vocabulary where the policy is.
	//
	// Options must be empty when this is set: there is nothing to reflect.
	PolicyOwned bool
}

// The option IDs Murtaugh answers a PolicyOwned request with. A backend that
// delegates the decision understands these three and nothing else; "" keeps its
// existing meaning of "nobody chose" (a timeout or a dismissal), which is a
// different outcome from a deliberate refusal and usually deserves a different
// message to the model.
const (
	PermissionAllow = "allow"
	PermissionDeny  = "deny"
)

// PermissionAsker resolves an ACP permission request by getting a human decision
// (e.g. via Slack buttons). It returns the chosen option's ID, or "" when no option
// was chosen (timeout / dismissal), which the ACP client maps to a "cancelled"
// outcome. Implemented in internal/slack/interaction; nil on headless/CLI paths.
type PermissionAsker interface {
	AskPermission(ctx context.Context, loc TurnLocation, req PermissionRequest) (optionID string, err error)
}

// PermissionPrompt is the EventPermission payload: a permission request plus the
// channel the consumer writes its decision back on. The ACP client raises the
// request on the event stream and blocks on Decision; the chat handler asks the
// human (after settling any open reply) and sends the chosen option's ID — or ""
// for a denial/timeout/dismissal, which the client maps to a "cancelled" outcome.
// Decision is buffered (cap 1) so the consumer's send never blocks; it must be
// written exactly once.
type PermissionPrompt struct {
	Request  PermissionRequest
	Decision chan string
}
