package agentwire

import "github.com/miere/murtaugh/internal/agent"

// PermissionGate exists because the native loop approves through a direct call, not an
// event; both approval paths ride this one frame so a native agent can run on a node.
type PermissionGate string

const (
	GateAgent PermissionGate = "agent"
	GateTool  PermissionGate = "tool"
)

type PermissionOption struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Kind string `json:"kind,omitempty"`
}

type PermissionRequest struct {
	ID          string             `json:"id"`
	Gate        PermissionGate     `json:"gate"`
	SessionID   string             `json:"session_id,omitempty"`
	ToolKind    string             `json:"tool_kind,omitempty"`
	ToolTitle   string             `json:"tool_title,omitempty"`
	PolicyOwned bool               `json:"policy_owned,omitempty"`
	Options     []PermissionOption `json:"options,omitempty"`
}

// An empty OptionID means nobody chose (a timeout or dismissal), which backends tell the model
// differently from a refusal. Note becomes a refused tool call's result.
type PermissionResponse struct {
	ID       string `json:"id"`
	OptionID string `json:"option_id,omitempty"`
	Note     string `json:"note,omitempty"`
}

func Response(id, optionID string) PermissionResponse {
	return PermissionResponse{ID: id, OptionID: optionID}
}

func (r PermissionResponse) Approval() (allowed bool, note string) {
	return r.OptionID == agent.PermissionAllow, r.Note
}

// NativeApproval is PolicyOwned because the native loop has no options of its own; the gateway
// supplies the buttons.
func NativeApproval(id, toolName, summary string) PermissionRequest {
	return PermissionRequest{
		ID:          id,
		Gate:        GateTool,
		ToolKind:    toolName,
		ToolTitle:   summary,
		PolicyOwned: true,
	}
}

type PendingDecision struct {
	ID string
	// Losing Gate fails silently: a GateTool request answered by the agent-harness asker loses
	// its note, which is the whole answer a native tool call gets.
	Gate     PermissionGate
	Decision <-chan string
}
