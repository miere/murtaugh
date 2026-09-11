package agentwire

import (
	"encoding/json"
	"fmt"

	"github.com/miere/murtaugh/internal/agent"
)

// EventType re-declares agent.EventType's strings instead of aliasing them: they are a
// contract between separately deployed processes, and the internal enum must stay free to change.
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

// Event's payload fields are pointers so an absent one is left out of the JSON, keeping
// "no task" distinct from "an empty task".
type Event struct {
	Type          EventType          `json:"type"`
	Text          string             `json:"text,omitempty"`
	StopReason    string             `json:"stop_reason,omitempty"`
	Error         *Error             `json:"error,omitempty"`
	Task          *Task              `json:"task,omitempty"`
	Attachment    *Attachment        `json:"attachment,omitempty"`
	Permission    *PermissionRequest `json:"permission,omitempty"`
	Question      *QuestionRequest   `json:"question,omitempty"`
	Plan          *PlanRequest       `json:"plan,omitempty"`
	SignIn        *SignInRequest     `json:"sign_in,omitempty"`
	SignInSettled *SignInSettled     `json:"sign_in_settled,omitempty"`
}

type TaskStatus string

const (
	TaskStatusPending    TaskStatus = "pending"
	TaskStatusInProgress TaskStatus = "in_progress"
	TaskStatusComplete   TaskStatus = "complete"
	TaskStatusFailed     TaskStatus = "failed"
	TaskStatusCancelled  TaskStatus = "cancelled"
)

// TaskKind must survive the wire because the renderer seals tool calls and plan entries
// differently; losing it would break streaming prose.
type TaskKind string

const (
	TaskKindTool TaskKind = ""
	TaskKindPlan TaskKind = "plan"
)

// Task carries Description though nothing reads it yet: two backends write it, and dropping
// it is a separate decision from making the event serialisable.
type Task struct {
	ID          string     `json:"id,omitempty"`
	Title       string     `json:"title,omitempty"`
	Status      TaskStatus `json:"status,omitempty"`
	Description string     `json:"description,omitempty"`
	Output      string     `json:"output,omitempty"`
	Kind        TaskKind   `json:"kind,omitempty"`
}

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
