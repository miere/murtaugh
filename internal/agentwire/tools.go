package agentwire

import "encoding/json"

// This file is the TOOL channel: node → gateway, the only direction that runs
// that way apart from a permission answer.
//
// It is a per-call RPC, not a tunnel of the MCP byte stream the in-process
// aggregator serves. That choice is the whole reason this file is short.
//
// #170 Concern 5 describes the tool channel as a stateful proxy because it
// assumes the MCP session crosses the network: the gateway end is an
// mcpsdk.ServerSession whose entire state IS the byte stream, and the SDK
// refuses every method but initialize/ping on a session that has not completed
// initialisation, so a reconnected pipe is dead rather than degraded. That is
// true — and it is a problem you only have if you move the pipe. Keep the MCP
// session on the node, where the agent's own subprocess dials a LOCAL unix
// socket, and send one frame per tool CALL instead: there is no session to
// replay, no initialise handshake to re-issue, and no MCP request id to rewrite.
// The link's own sequencing, acknowledgement and ordering (internal/nodelink)
// are the delivery guarantee, rather than a second weaker one layered above it.
//
// It also fixes the backend the tunnel would have missed. A native agent — the
// default one — never touches the aggregator at all; it resolves a []tools.Tool
// and calls Invoke directly. Tunnelling the MCP stream would have restored
// Murtaugh's tools for acp and claude_code and left native tool-less, which is
// half a regression fix and the harder half to notice.
//
// What crosses is deliberately narrow: a description of a tool, a call, and a
// result. Nothing here can express "run this on the gateway with these
// credentials" other than by naming a tool the gateway already decided a node
// may reach — see toolset.Reach, and internal/nodehost, which is the single
// place that decision is enforced.

// ToolDescriptor is one tool the gateway offers a node, in the terms the node
// needs to republish it to its own agent.
//
// It carries no approval information on purpose. Whether a call needs a human is
// decided per call from its ARGUMENTS (tools.ApprovalClassifier), by the tool
// itself, on the side that holds the tool — and the human being asked is the
// gateway's user either way. A boolean copied onto this struct would be a second
// opinion that drifts from the first, and the node gating as well as the gateway
// would prompt one person twice for one call.
type ToolDescriptor struct {
	// Name is the registry key — "slack.send-msg", "ping". It is what a
	// ToolCall names and what the gateway looks up.
	Name string `json:"name"`
	// PublishedName is the tool's MCPName override, empty when it has none.
	//
	// It is carried because dropping it is invisible: `ask` publishes as
	// AskUserQuestion so a Claude Code agent reaches for it by reflex instead of
	// having to be taught a new name. A proxy that republished it as `ask` would
	// not fail — the agent would simply stop asking, and nothing would say so.
	PublishedName string `json:"published_name,omitempty"`
	Description   string `json:"description,omitempty"`
	// InputSchema is the tool's JSON Schema, carried raw. This package has no
	// opinion about schema dialects and should not grow one; the node
	// unmarshals it back into the same type the gateway marshalled.
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// ToolListResult answers MethodToolList.
//
// An empty list is a legitimate answer and means "you may reach nothing" — a
// node whose partition grants it nothing, which is a policy outcome rather than
// a failure. A gateway that cannot answer at all faults instead, so the node
// can refuse to publish an agent with a silently empty toolset.
type ToolListResult struct {
	Tools []ToolDescriptor `json:"tools"`
}

// ToolCall is MethodToolCall's request: one invocation, on the gateway.
type ToolCall struct {
	// Stream is the id of the turn this call belongs to, and it is not
	// bookkeeping. The gateway re-injects the turn's agent.TurnLocation from it
	// before invoking, and four registry tools (ask, present_plan, auth.request
	// and the approval gate itself) read that location off the context and
	// degrade to HEADLESS without it — headless meaning ungated, or silently
	// non-interactive. A call that arrived with no stream would run with nobody
	// asked and no thread to answer in.
	Stream string `json:"stream,omitempty"`
	// Name is the tool's registry key.
	Name string `json:"name"`
	// Args are keyed by the tool's own input schema properties.
	Args map[string]any `json:"args,omitempty"`
}

// ToolResult answers MethodToolCall.
//
// A DENIAL is a result, not a fault: when the human refuses, the note is the
// call's result string handed to the model, exactly as it is in process
// (internal/frontends/mcp gate, internal/agent/native loop). A fault means the
// call did not happen for a reason the model should read as an error.
type ToolResult struct {
	// Content is the tool's rendered output. Strings pass through unchanged;
	// everything else arrives already JSON-encoded, which is the same shape both
	// in-process consumers produce.
	Content string `json:"content"`
}
