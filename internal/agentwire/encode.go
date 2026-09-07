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

// Encoder translates the agent event stream into wire events. It runs on the
// producing side — a runtime node.
//
// It holds state for exactly one reason: a permission request is a request, and
// the backend goroutine that raised it is blocked on a channel the wire cannot
// carry. The Encoder keeps that channel under the correlation id it minted and
// Resolve delivers the answer to it. Everything else is a pure translation.
//
// Safe for concurrent use: a session's events are produced by one goroutine but
// answers arrive on the connection's.
type Encoder struct {
	mu      sync.Mutex
	pending map[string]chan string
}

// NewEncoder returns an Encoder with no outstanding requests.
func NewEncoder() *Encoder {
	return &Encoder{pending: make(map[string]chan string)}
}

// Encode translates one agent event into its wire form.
//
// The second result is the side channel the event needs and the frame cannot
// carry: an attachment's bytes. It is nil for every other kind, and when it is
// non-nil the caller owns it and must close its Body.
//
// An unknown event kind is an error rather than a dropped event: a kind added to
// internal/agent without being taught to the wire must fail loudly here, not
// vanish somewhere between a node and a user.
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
	default:
		return Event{}, nil, fmt.Errorf("agentwire: unknown event kind %q", ev.Type)
	}
}

// track mints a correlation id for the prompt, remembers the channel its
// backend is blocked on, and returns the request to put on the wire.
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

// Resolve delivers an answer to the backend blocked on the request it names.
//
// An unknown id is an error: the request was already answered, abandoned, or
// never existed, and answering nothing quietly would leave a turn hanging with
// no trace. The send is non-blocking because the prompt's channel is buffered
// (cap 1) and written exactly once — the same contract the in-process consumer
// works to.
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

// Abandon forgets a request without answering it, for turn teardown: the
// backend's own ctx.Done escape has already decided the outcome (a deny), so
// the entry would otherwise outlive the goroutine waiting on it.
func (e *Encoder) Abandon(id string) {
	e.mu.Lock()
	delete(e.pending, id)
	e.mu.Unlock()
}

// Pending reports how many permission requests are outstanding. It exists for
// tests and for a health surface: a number that only grows is a leak.
func (e *Encoder) Pending() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.pending)
}

// encodeAttachment produces the metadata frame and opens the byte stream that
// accompanies it. Data wins over Path when both are set, matching
// agent.AttachmentEvent's documented precedence.
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
		// Opened and stat'd through the same handle so the size on the wire is
		// the size of the bytes that will actually be sent, not of whatever the
		// path pointed at a moment earlier.
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
		// The in-process handler drops this case silently. On a wire it must not
		// be silent: an attachment with no bytes is a turn that promised the user
		// a file and will not deliver one.
		return Attachment{}, nil, fmt.Errorf("agentwire: attachment %q has neither data nor a path", a.Filename)
	}
}
