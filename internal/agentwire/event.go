package agentwire

import (
	"encoding/json"
	"fmt"

	"github.com/miere/murtaugh/internal/agent"
)

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
	EventQuestion   EventType = "question"
	EventPlan       EventType = "plan"
	EventSignIn     EventType = "sign_in"
	// EventSignInSettled travels node to gateway on the turn's stream, in order,
	// so the cards settle before the turn that raised the sign-in completes.
	EventSignInSettled EventType = "sign_in_settled"
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
	Permission    *PermissionRequest `json:"permission,omitempty"`
	Question      *QuestionRequest   `json:"question,omitempty"`
	Plan          *PlanRequest       `json:"plan,omitempty"`
	SignIn        *SignInRequest     `json:"sign_in,omitempty"`
	SignInSettled *SignInSettled     `json:"sign_in_settled,omitempty"`
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

const MessageAnswer MessageKind = "answer"

// QuestionRequest has no destination field by design: the gateway draws it in
// the conversation the turn belongs to, so a node cannot aim a card anywhere.
type QuestionRequest struct {
	ID        string     `json:"id"`
	Title     string     `json:"title,omitempty"`
	Questions []Question `json:"questions"`
}

type Question struct {
	Key         string           `json:"key"`
	Header      string           `json:"header,omitempty"`
	Question    string           `json:"question"`
	Options     []QuestionOption `json:"options,omitempty"`
	MultiSelect bool             `json:"multi_select,omitempty"`
}

type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// PlanRequest has no destination field, for the same reason QuestionRequest has none.
type PlanRequest struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	Plan  string `json:"plan"`
}

// DisplayAnswer travels gateway to node under the id the node minted, like a
// PermissionResponse, because it answers an event rather than a call.
type DisplayAnswer struct {
	ID      string              `json:"id"`
	Outcome string              `json:"outcome"`
	Answers map[string][]string `json:"answers,omitempty"`
	Choice  string              `json:"choice,omitempty"`
	UserID  string              `json:"user_id,omitempty"`
	Note    string              `json:"note,omitempty"`
	// Code is what the node's owner typed into the sign-in card, for the node's
	// own sign-in process; nothing the gateway holds rides with it.
	Code string `json:"code,omitempty"`
}

// SignInRequest has no destination and no environment: the node signs in with
// its own environment, and the gateway decides where the cards go.
type SignInRequest struct {
	ID        string `json:"id"`
	Tool      string `json:"tool"`
	Profile   string `json:"profile"`
	URL       string `json:"url"`
	NeedsCode bool   `json:"needs_code,omitempty"`
	// Command is what the owner is asked to approve before it runs, which is
	// why a request carrying one has no URL yet.
	Command string `json:"command,omitempty"`
}

// SignInSettled expects no answer, because the node's own sign-in process is
// what decides how a sign-in ends.
type SignInSettled struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	URL    string `json:"url,omitempty"`
}

func AnswerDisplay(a DisplayAnswer) (Message, error) {
	encoded, err := json.Marshal(a)
	if err != nil {
		return Message{}, fmt.Errorf("agentwire: encode display answer: %w", err)
	}
	return Message{Kind: MessageAnswer, ID: a.ID, Body: encoded}, nil
}

func EncodeDisplayAnswer(id string, a agent.DisplayAnswer) DisplayAnswer {
	return DisplayAnswer{
		ID:      id,
		Outcome: string(a.Outcome),
		Answers: a.Answers,
		Choice:  a.Choice,
		UserID:  a.UserID,
		Note:    a.Note,
		Code:    a.Code,
	}
}

func (a DisplayAnswer) Decode() agent.DisplayAnswer {
	return agent.DisplayAnswer{
		Outcome: agent.DisplayOutcome(a.Outcome),
		Answers: a.Answers,
		Choice:  a.Choice,
		UserID:  a.UserID,
		Note:    a.Note,
		Code:    a.Code,
	}
}

func encodeQuestion(id string, r agent.QuestionRequest) QuestionRequest {
	questions := make([]Question, 0, len(r.Questions))
	for _, q := range r.Questions {
		var options []QuestionOption
		for _, o := range q.Options {
			options = append(options, QuestionOption{Label: o.Label, Description: o.Description})
		}
		questions = append(questions, Question{
			Key:         q.Key,
			Header:      q.Header,
			Question:    q.Question,
			Options:     options,
			MultiSelect: q.MultiSelect,
		})
	}
	return QuestionRequest{ID: id, Title: r.Title, Questions: questions}
}

func decodeQuestion(w QuestionRequest) agent.QuestionRequest {
	questions := make([]agent.Question, 0, len(w.Questions))
	for _, q := range w.Questions {
		var options []agent.QuestionOption
		for _, o := range q.Options {
			options = append(options, agent.QuestionOption{Label: o.Label, Description: o.Description})
		}
		questions = append(questions, agent.Question{
			Key:         q.Key,
			Header:      q.Header,
			Question:    q.Question,
			Options:     options,
			MultiSelect: q.MultiSelect,
		})
	}
	return agent.QuestionRequest{Title: w.Title, Questions: questions}
}
