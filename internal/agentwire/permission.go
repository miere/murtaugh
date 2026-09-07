package agentwire

import "github.com/miere/murtaugh/internal/agent"

// PermissionGate names which of the gateway's two approval surfaces answers a
// request.
//
// It exists because the in-process codebase has TWO approval paths and only one
// of them is an event. The ACP and claude_code backends raise EventPermission
// and block on a channel; the native loop — the DEFAULT backend — calls an
// Approver interface synchronously and never touches the event stream at all
// (internal/agent/native/loop.go). Move a native agent onto a node and that call
// has nothing to reach. A request/response design derived only from
// EventPermission would cover two backends out of three, so both ride this one
// frame.
//
// The gateway keeps the decision either way. Its always-allow Grants set is
// shared between the two surfaces so a grant made on one suppresses the prompt
// on the other, and a grant hit short-circuits before anything is posted — which
// is a strong argument for sending only a REQUEST across the wire and never the
// authority to answer it.
type PermissionGate string

const (
	// GateAgent — the agent's own harness is asking: an ACP
	// session/request_permission, or claude_code's can_use_tool. Answered by
	// the agent's PermissionAsker.
	GateAgent PermissionGate = "agent"
	// GateTool — the native loop is gating a side-effecting tool call inline.
	// Answered by the gateway's tool approval gate, whose reply is a (allowed,
	// note) pair rather than an option id; see PermissionResponse.Approval.
	GateTool PermissionGate = "tool"
)

// PermissionOption is the serialisable form of agent.PermissionOption: one
// choice the agent itself offers, echoed back by ID.
type PermissionOption struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Kind string `json:"kind,omitempty"`
}

// PermissionRequest is the request half of the pair that replaces
// agent.PermissionPrompt's channel.
//
// ID is the correlation identifier: minted by the node, unique for as long as
// the request is outstanding, and the only thing tying the answer back to the
// backend goroutine blocked on it. It has no counterpart in agent.Event, which
// did not need one because the channel WAS the correlation.
type PermissionRequest struct {
	ID          string             `json:"id"`
	Gate        PermissionGate     `json:"gate"`
	SessionID   string             `json:"session_id,omitempty"`
	ToolKind    string             `json:"tool_kind,omitempty"`
	ToolTitle   string             `json:"tool_title,omitempty"`
	PolicyOwned bool               `json:"policy_owned,omitempty"`
	Options     []PermissionOption `json:"options,omitempty"`
}

// PermissionResponse is the answer frame. It is deliberately NOT an Event: the
// event stream is one-way and ordered per turn, and an answer is neither. This
// is the field that proves the protocol carries request/response and not only
// events.
//
// OptionID is an agent-declared option id, or one of agent.PermissionAllow /
// agent.PermissionDeny for a PolicyOwned request, or "" — which keeps its
// existing meaning of "nobody chose" (a timeout or a dismissal). That is
// deliberately distinct from a deliberate refusal: both backends say something
// different to the model for the two, so collapsing them would change what the
// agent is told.
//
// Note is the sentence handed back to the model when a GateTool request is
// refused. The native Approver returns (bool, string) and the string is not
// diagnostics — it becomes the tool call's result.
type PermissionResponse struct {
	ID       string `json:"id"`
	OptionID string `json:"option_id,omitempty"`
	Note     string `json:"note,omitempty"`
}

// Response builds an answer to the request identified by id.
func Response(id, optionID string) PermissionResponse {
	return PermissionResponse{ID: id, OptionID: optionID}
}

// Approval renders this response as the pair the native loop's Approver
// contract returns: whether to run the tool, and the note to feed the model when
// not.
func (r PermissionResponse) Approval() (allowed bool, note string) {
	return r.OptionID == agent.PermissionAllow, r.Note
}

// NativeApproval builds the request a native agent raises when its inline
// approval gate fires. toolName is the tool being gated and summary is what the
// human is shown — for the terminal tool, the command line.
//
// It is PolicyOwned because the native loop has no options of its own: the
// gateway supplies the buttons (approve / approve & always allow / deny), which
// is exactly the shape claude_code's can_use_tool already takes.
func NativeApproval(id, toolName, summary string) PermissionRequest {
	return PermissionRequest{
		ID:          id,
		Gate:        GateTool,
		ToolKind:    toolName,
		ToolTitle:   summary,
		PolicyOwned: true,
	}
}

// PendingDecision is the side channel a decoded permission request needs and
// the frame cannot carry: the channel the consumer writes its decision to, and
// the id that turns that decision into a PermissionResponse.
//
// The codec does not read it. Whatever owns the connection reads the channel and
// sends Response(ID, decision) back — that goroutine belongs to the transport,
// not to a translation.
type PendingDecision struct {
	ID       string
	Decision <-chan string
}
