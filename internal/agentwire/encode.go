package agentwire

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/google/uuid"

	"github.com/miere/murtaugh/internal/agent"
)

type Encoder struct {
	mu       sync.Mutex
	pending  map[string]chan string
	displays map[string]display
	signIns  map[*agent.SignInPrompt]string
}

type display struct {
	answer chan agent.DisplayAnswer
	open   bool
}

func NewEncoder() *Encoder {
	return &Encoder{
		pending:  make(map[string]chan string),
		displays: make(map[string]display),
		signIns:  make(map[*agent.SignInPrompt]string),
	}
}

// The caller must close the returned Transfer's Body. An unknown kind fails loudly so a kind
// added to internal/agent never vanishes between node and user.
func (e *Encoder) Encode(ev agent.Event) (Event, *Transfer, error) {
	switch ev.Type {
	case agent.EventText:
		return Event{Type: EventText, Text: ev.Text}, nil, nil
	case agent.EventStatus:
		return Event{Type: EventStatus, Text: ev.Text}, nil, nil
	case agent.EventComplete:
		return Event{Type: EventComplete, Text: ev.Text, StopReason: ev.StopReason}, nil, nil
	case agent.EventError:
		return Event{Type: EventError, Text: ev.Text, Error: encodeError(ev.Error)}, nil, nil
	case agent.EventTask:
		if ev.Task == nil {
			return Event{}, nil, fmt.Errorf("agentwire: task event carries no task")
		}
		return Event{Type: EventTask, Task: &Task{
			ID:          ev.Task.ID,
			Title:       ev.Task.Title,
			Status:      TaskStatus(ev.Task.Status),
			Description: ev.Task.Description,
			Output:      ev.Task.Output,
			Kind:        TaskKind(ev.Task.Kind),
		}}, nil, nil
	case agent.EventAttachment:
		if ev.Attachment == nil {
			return Event{}, nil, fmt.Errorf("agentwire: attachment event carries no attachment")
		}
		a, transfer, err := encodeAttachment(ev.Attachment)
		if err != nil {
			return Event{}, nil, err
		}
		return Event{Type: EventAttachment, Attachment: &a}, transfer, nil
	case agent.EventPermission:
		if ev.Permission == nil {
			return Event{}, nil, fmt.Errorf("agentwire: permission event carries no prompt")
		}
		req := e.track(ev.Permission)
		return Event{Type: EventPermission, Permission: &req}, nil, nil
	case agent.EventQuestion:
		if ev.Question == nil {
			return Event{}, nil, fmt.Errorf("agentwire: question event carries no prompt")
		}
		req := encodeQuestion(e.trackDisplay(ev.Question.Answer), ev.Question.Request)
		return Event{Type: EventQuestion, Question: &req}, nil, nil
	case agent.EventPlan:
		if ev.Plan == nil {
			return Event{}, nil, fmt.Errorf("agentwire: plan event carries no prompt")
		}
		id := e.trackDisplay(ev.Plan.Answer)
		return Event{Type: EventPlan, Plan: &PlanRequest{ID: id, Title: ev.Plan.Request.Title, Plan: ev.Plan.Request.Plan}}, nil, nil
	case agent.EventSignIn:
		if ev.SignIn == nil {
			return Event{}, nil, fmt.Errorf("agentwire: sign-in event carries no prompt")
		}
		r := ev.SignIn.Request
		return Event{Type: EventSignIn, SignIn: &SignInRequest{
			ID:        e.trackSignIn(ev.SignIn),
			Tool:      r.Tool,
			Profile:   r.Profile,
			URL:       r.URL,
			NeedsCode: r.NeedsCode,
			Command:   r.Command,
		}}, nil, nil
	case agent.EventSignInSettled:
		if ev.SignInSettled == nil {
			return Event{}, nil, fmt.Errorf("agentwire: sign-in settle carries no state")
		}
		id, err := e.settleSignIn(ev.SignInSettled)
		if err != nil {
			return Event{}, nil, err
		}
		return Event{Type: EventSignInSettled, SignInSettled: &SignInSettled{
			ID:     id,
			State:  string(ev.SignInSettled.State),
			Reason: ev.SignInSettled.Reason,
			URL:    ev.SignInSettled.URL,
		}}, nil, nil
	default:
		return Event{}, nil, fmt.Errorf("agentwire: unknown event kind %q", ev.Type)
	}
}

func (e *Encoder) track(p *agent.PermissionPrompt) PermissionRequest {
	id := uuid.NewString()
	e.mu.Lock()
	e.pending[id] = p.Decision
	e.mu.Unlock()

	options := make([]PermissionOption, 0, len(p.Request.Options))
	for _, o := range p.Request.Options {
		options = append(options, PermissionOption{ID: o.ID, Name: o.Name, Kind: o.Kind})
	}
	if len(options) == 0 {
		options = nil
	}
	return PermissionRequest{
		ID:          id,
		Gate:        GateAgent,
		SessionID:   p.Request.SessionID,
		ToolKind:    p.Request.ToolKind,
		ToolTitle:   p.Request.ToolTitle,
		PolicyOwned: p.Request.PolicyOwned,
		Options:     options,
	}
}

func (e *Encoder) trackDisplay(answer chan agent.DisplayAnswer) string {
	id := uuid.NewString()
	e.mu.Lock()
	e.displays[id] = display{answer: answer}
	e.mu.Unlock()
	return id
}

func (e *Encoder) trackSignIn(p *agent.SignInPrompt) string {
	id := uuid.NewString()
	e.mu.Lock()
	e.displays[id] = display{answer: p.Answer, open: true}
	e.signIns[p] = id
	e.mu.Unlock()
	return id
}

func (e *Encoder) settleSignIn(s *agent.SignInSettled) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	id, ok := e.signIns[s.Prompt]
	if !ok {
		return "", fmt.Errorf("agentwire: no sign-in pending to settle as %q", s.State)
	}
	if s.State.Terminal() {
		delete(e.signIns, s.Prompt)
		delete(e.displays, id)
	}
	return id, nil
}

// Answer fails on an unknown id for the reason Resolve does: answering nothing
// quietly would leave a tool waiting with no trace.
func (e *Encoder) Answer(a DisplayAnswer) error {
	e.mu.Lock()
	pending, ok := e.displays[a.ID]
	if !pending.open {
		delete(e.displays, a.ID)
	}
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("agentwire: no question, plan or sign-in pending for id %q", a.ID)
	}
	if pending.answer == nil {
		return nil
	}
	select {
	case pending.answer <- a.Decode():
	default:
	}
	return nil
}

// Resolve fails on an unknown id because answering nothing quietly would leave a turn
// hanging with no trace.
func (e *Encoder) Resolve(resp PermissionResponse) error {
	e.mu.Lock()
	decision, ok := e.pending[resp.ID]
	delete(e.pending, resp.ID)
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("agentwire: no permission request pending for id %q", resp.ID)
	}
	if decision == nil {
		return nil
	}
	select {
	case decision <- resp.OptionID:
	default:
	}
	return nil
}

// Abandon exists for turn teardown: the backend's ctx.Done has already denied, and the entry
// would otherwise outlive the goroutine waiting on it.
func (e *Encoder) Abandon(id string) {
	e.mu.Lock()
	delete(e.pending, id)
	delete(e.displays, id)
	for prompt, held := range e.signIns {
		if held == id {
			delete(e.signIns, prompt)
		}
	}
	e.mu.Unlock()
}

// Pending exists for tests and for a health surface: a number that only grows
// is a leak.
func (e *Encoder) Pending() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.pending) + len(e.displays)
}

func encodeAttachment(a *agent.AttachmentEvent) (Attachment, *Transfer, error) {
	w := Attachment{
		Filename:   a.Filename,
		Title:      a.Title,
		Comment:    a.Comment,
		Mimetype:   a.Mimetype,
		TransferID: uuid.NewString(),
	}
	switch {
	case len(a.Data) > 0:
		w.Size = int64(len(a.Data))
		return w, &Transfer{ID: w.TransferID, Size: w.Size, Body: io.NopCloser(bytes.NewReader(a.Data))}, nil
	case a.Path != "":
		f, err := os.Open(a.Path)
		if err != nil {
			return Attachment{}, nil, fmt.Errorf("open attachment %q: %w", a.Path, err)
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return Attachment{}, nil, fmt.Errorf("stat attachment %q: %w", a.Path, err)
		}
		w.Size = info.Size()
		return w, &Transfer{ID: w.TransferID, Size: w.Size, Body: f}, nil
	default:
		return Attachment{}, nil, fmt.Errorf("agentwire: attachment %q has neither data nor a path", a.Filename)
	}
}
