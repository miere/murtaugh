package agentwire

// EventType is the wire's own vocabulary of event kinds.
//
// The strings match agent.EventType's today, and are re-declared rather than
// aliased on purpose: these are a published contract between two independently
// deployed processes, while the internal enum must stay free to change. Encoder
// and Decoder are the only places the two meet, and the exhaustiveness test in
// roundtrip_test.go is what notices when a kind is added to one and not the
// other: it reads internal/agent's constants out of that package's source, so
// it cannot itself drift into agreeing with a stale copy of the enum.
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

// Event is the serialisable form of agent.Event: one item on the ordered stream
// a turn emits.
//
// JSON naming follows the house convention for Murtaugh-owned wire shapes —
// snake_case with omitempty (see internal/journal). agent.SessionMetadata's
// camelCase tags are not a precedent: nothing marshals it anywhere, so they
// have never been a contract with anything.
//
// The payload fields are pointers so an absent one is absent from the JSON
// rather than present and empty, which keeps a frame legible in a log and keeps
// "no task on this event" distinguishable from "an empty task".
type Event struct {
	Type       EventType   `json:"type"`
	Text       string      `json:"text,omitempty"`
	StopReason string      `json:"stop_reason,omitempty"`
	Error      *Error      `json:"error,omitempty"`
	Task       *Task       `json:"task,omitempty"`
	Attachment *Attachment `json:"attachment,omitempty"`
	// Permission is the request half of the request/response pair that replaces
	// agent.PermissionPrompt's channel. The answer is not an Event at all; it
	// comes back as its own PermissionResponse frame.
	Permission *PermissionRequest `json:"permission,omitempty"`
}

// TaskStatus mirrors agent.TaskStatus.
type TaskStatus string

const (
	TaskStatusPending    TaskStatus = "pending"
	TaskStatusInProgress TaskStatus = "in_progress"
	TaskStatusComplete   TaskStatus = "complete"
	TaskStatusFailed     TaskStatus = "failed"
	TaskStatusCancelled  TaskStatus = "cancelled"
)

// TaskKind mirrors agent.TaskKind: a tool invocation (the zero value) or an
// entry in the agent's plan snapshot. The renderer seals differently for the
// two, so losing it would shred streaming prose on a remote node exactly as it
// would in process.
type TaskKind string

const (
	TaskKindTool TaskKind = ""
	TaskKindPlan TaskKind = "plan"
)

// Task is the serialisable form of agent.TaskEvent.
//
// Description carries even though no consumer reads it today: two backends
// populate it, and dropping a field that live producers write is a separate
// decision from making the abstraction serialisable. A wire that silently
// narrows the abstraction is not a translation of it.
type Task struct {
	ID          string     `json:"id,omitempty"`
	Title       string     `json:"title,omitempty"`
	Status      TaskStatus `json:"status,omitempty"`
	Description string     `json:"description,omitempty"`
	Output      string     `json:"output,omitempty"`
	Kind        TaskKind   `json:"kind,omitempty"`
}

// maxFrameBytes is the single-frame ceiling this repository already applies to
// both of its agent protocols (the ACP transport's and claude_code's NDJSON
// scanners are both built with an 8 MiB cap). It is not enforced here — this
// package does no framing — but the attachment decision was made against it,
// and TestTransferChunkRoundTrip holds the chunk size to it.
const maxFrameBytes = 8 << 20
