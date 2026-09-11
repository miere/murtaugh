package agentwire

import (
	"context"
	"fmt"
	"sync"

	"github.com/miere/murtaugh/internal/agent"
)

// Decoder translates wire events back into agent events. It runs on the
// consuming side — the gateway — and produces exactly the values the renderer,
// chat handler, session log and delegate runner already consume, so nothing
// downstream can tell whether the turn ran in this process or on a node.
type Decoder struct {
	deliver AttachmentDeliverer

	mu      sync.Mutex
	signIns map[string]*agent.SignInPrompt
}

// NewDecoder returns a Decoder that materialises attachments through deliver. A
// nil deliverer is legal for a caller that will never see an attachment (and
// makes one an error if it does), which is what the error-path tests use.
func NewDecoder(deliver AttachmentDeliverer) *Decoder {
	return &Decoder{deliver: deliver, signIns: make(map[string]*agent.SignInPrompt)}
}

// ForgetSignIn is for a gateway that stopped drawing a sign-in before the node
// settled it; a later settle for it then decodes as an error.
func (d *Decoder) ForgetSignIn(id string) {
	d.mu.Lock()
	delete(d.signIns, id)
	d.mu.Unlock()
}

// SignInOpen lets a caller drop a stale settle quietly instead of reporting it.
func (d *Decoder) SignInOpen(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.signIns[id]
	return ok
}

// Decode translates one wire event back into an agent event.
//
// The second result is the side channel a decoded event needs: for a permission
// request, the channel the consumer answers on together with the id that answer
// must be sent back under. It is nil for every other kind.
//
// An unknown kind is an error for the same reason it is on the way out: a node
// running a newer build must not have its events quietly discarded.
func (d *Decoder) Decode(ctx context.Context, w Event) (agent.Event, *PendingDecision, error) {
	switch w.Type {
	case EventText:
		return agent.Event{Type: agent.EventText, Text: w.Text}, nil, nil
	case EventStatus:
		return agent.Event{Type: agent.EventStatus, Text: w.Text}, nil, nil
	case EventComplete:
		return agent.Event{Type: agent.EventComplete, Text: w.Text, StopReason: w.StopReason}, nil, nil
	case EventError:
		return agent.Event{Type: agent.EventError, Text: w.Text, Error: decodeError(w.Error)}, nil, nil
	case EventTask:
		if w.Task == nil {
			return agent.Event{}, nil, fmt.Errorf("agentwire: task event carries no task")
		}
		return agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{
			ID:          w.Task.ID,
			Title:       w.Task.Title,
			Status:      agent.TaskStatus(w.Task.Status),
			Description: w.Task.Description,
			Output:      w.Task.Output,
			Kind:        agent.TaskKind(w.Task.Kind),
		}}, nil, nil
	case EventAttachment:
		if w.Attachment == nil {
			return agent.Event{}, nil, fmt.Errorf("agentwire: attachment event carries no attachment")
		}
		a, err := d.decodeAttachment(ctx, *w.Attachment)
		if err != nil {
			return agent.Event{}, nil, err
		}
		return agent.Event{Type: agent.EventAttachment, Attachment: a}, nil, nil
	case EventPermission:
		if w.Permission == nil {
			return agent.Event{}, nil, fmt.Errorf("agentwire: permission event carries no request")
		}
		ev, pending := decodePermission(*w.Permission)
		return ev, pending, nil
	case EventQuestion:
		if w.Question == nil {
			return agent.Event{}, nil, fmt.Errorf("agentwire: question event carries no request")
		}
		answer := make(chan agent.DisplayAnswer, 1)
		ev := agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionPrompt{Request: decodeQuestion(*w.Question), Answer: answer}}
		return ev, &PendingDecision{ID: w.Question.ID}, nil
	case EventPlan:
		if w.Plan == nil {
			return agent.Event{}, nil, fmt.Errorf("agentwire: plan event carries no request")
		}
		answer := make(chan agent.DisplayAnswer, 1)
		ev := agent.Event{Type: agent.EventPlan, Plan: &agent.PlanPrompt{
			Request: agent.PlanRequest{Title: w.Plan.Title, Plan: w.Plan.Plan},
			Answer:  answer,
		}}
		return ev, &PendingDecision{ID: w.Plan.ID}, nil
	case EventSignIn:
		if w.SignIn == nil {
			return agent.Event{}, nil, fmt.Errorf("agentwire: sign-in event carries no request")
		}
		prompt := &agent.SignInPrompt{
			Request: agent.SignInRequest{Tool: w.SignIn.Tool, Profile: w.SignIn.Profile, URL: w.SignIn.URL, NeedsCode: w.SignIn.NeedsCode, Command: w.SignIn.Command},
			Answer:  make(chan agent.DisplayAnswer, 2),
		}
		d.mu.Lock()
		d.signIns[w.SignIn.ID] = prompt
		d.mu.Unlock()
		return agent.Event{Type: agent.EventSignIn, SignIn: prompt}, &PendingDecision{ID: w.SignIn.ID}, nil
	case EventSignInSettled:
		if w.SignInSettled == nil {
			return agent.Event{}, nil, fmt.Errorf("agentwire: sign-in settle carries no state")
		}
		state := agent.SignInState(w.SignInSettled.State)
		d.mu.Lock()
		prompt, ok := d.signIns[w.SignInSettled.ID]
		if ok && state.Terminal() {
			delete(d.signIns, w.SignInSettled.ID)
		}
		d.mu.Unlock()
		if !ok {
			return agent.Event{}, nil, fmt.Errorf("agentwire: no sign-in open for id %q", w.SignInSettled.ID)
		}
		return agent.Event{Type: agent.EventSignInSettled, SignInSettled: &agent.SignInSettled{
			Prompt: prompt,
			State:  state,
			Reason: w.SignInSettled.Reason,
			URL:    w.SignInSettled.URL,
		}}, nil, nil
	default:
		return agent.Event{}, nil, fmt.Errorf("agentwire: unknown event kind %q", w.Type)
	}
}

// decodePermission rebuilds the prompt the consumer expects, with a fresh
// channel in place of the one that could not cross.
//
// The channel is buffered (cap 1) and written exactly once, exactly as both
// backends construct it in process, so the consumer's send never blocks even if
// the transport has already given up on the request.
func decodePermission(req PermissionRequest) (agent.Event, *PendingDecision) {
	options := make([]agent.PermissionOption, 0, len(req.Options))
	for _, o := range req.Options {
		options = append(options, agent.PermissionOption{ID: o.ID, Name: o.Name, Kind: o.Kind})
	}
	if len(options) == 0 {
		options = nil
	}
	decision := make(chan string, 1)
	ev := agent.Event{Type: agent.EventPermission, Permission: &agent.PermissionPrompt{
		Request: agent.PermissionRequest{
			SessionID:   req.SessionID,
			ToolKind:    req.ToolKind,
			ToolTitle:   req.ToolTitle,
			Options:     options,
			PolicyOwned: req.PolicyOwned,
		},
		Decision: decision,
	}}
	return ev, &PendingDecision{ID: req.ID, Gate: req.Gate, Decision: decision}
}

// decodeAttachment materialises the side transfer into the local byte source the
// uploader consumes.
func (d *Decoder) decodeAttachment(ctx context.Context, w Attachment) (*agent.AttachmentEvent, error) {
	if d.deliver == nil {
		return nil, fmt.Errorf("agentwire: attachment %q arrived with no deliverer to fetch transfer %q", w.Filename, w.TransferID)
	}
	path, data, err := d.deliver.DeliverAttachment(ctx, w)
	if err != nil {
		return nil, fmt.Errorf("deliver attachment %q: %w", w.Filename, err)
	}
	if path == "" && len(data) == 0 {
		return nil, fmt.Errorf("agentwire: transfer %q delivered neither bytes nor a path for %q", w.TransferID, w.Filename)
	}
	return &agent.AttachmentEvent{
		Filename: w.Filename,
		Title:    w.Title,
		Comment:  w.Comment,
		Mimetype: w.Mimetype,
		Path:     path,
		Data:     data,
	}, nil
}
