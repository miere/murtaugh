package gateway

import (
	"context"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/slack/alertcard"
)

// recordingRenderer is a chatRenderer that records what it was driven with.
type recordingRenderer struct {
	text     string
	tasks    int
	finished bool
	failed   error
	stopped  bool
}

func (r *recordingRenderer) Text(context.Context, string) error {
	return nil
}
func (r *recordingRenderer) Task(context.Context, *agent.TaskEvent) error             { r.tasks++; return nil }
func (r *recordingRenderer) Attachment(context.Context, *agent.AttachmentEvent) error { return nil }
func (r *recordingRenderer) BeginInterjection(context.Context)                        {}
func (r *recordingRenderer) Finish(context.Context, *alertcard.Spec) error {
	r.finished = true
	return nil
}
func (r *recordingRenderer) Fail(_ context.Context, err error) error { r.failed = err; return nil }
func (r *recordingRenderer) Interrupted(context.Context)             {}
func (r *recordingRenderer) EnsureStopped(context.Context)           { r.stopped = true }

func TestBackgroundSinkRendersRegisteredSession(t *testing.T) {
	sink := newBackgroundEventsRouter(nil)
	var made []*recordingRenderer
	sink.bind(func(config.ProgressDisplay, string, string, StreamWriterOptions) chatRenderer {
		r := &recordingRenderer{}
		made = append(made, r)
		return r
	})
	sink.Register("sess", bgTarget{channelID: "C", threadTS: "1"})

	sink.Handle("sess", agent.Event{Type: agent.EventText, Text: "hello"})
	sink.Handle("sess", agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "t1"}})
	sink.Handle("sess", agent.Event{Type: agent.EventComplete})

	if len(made) != 1 {
		t.Fatalf("expected exactly one renderer for the turn, got %d", len(made))
	}
	r := made[0]
	if r.tasks != 1 {
		t.Errorf("task not rendered: tasks=%d", r.tasks)
	}
	if !r.finished || !r.stopped {
		t.Errorf("turn not finalised: finished=%v stopped=%v", r.finished, r.stopped)
	}
}

func TestBackgroundSinkDropsUnregisteredSession(t *testing.T) {
	sink := newBackgroundEventsRouter(nil)
	built := false
	sink.bind(func(config.ProgressDisplay, string, string, StreamWriterOptions) chatRenderer {
		built = true
		return &recordingRenderer{}
	})
	// No Register → no target → the event is dropped, no renderer built.
	sink.Handle("ghost", agent.Event{Type: agent.EventText, Text: "x"})
	if built {
		t.Fatal("built a renderer for an unregistered session")
	}
}

func TestBackgroundSinkFreshRendererPerTurn(t *testing.T) {
	sink := newBackgroundEventsRouter(nil)
	built := 0
	sink.bind(func(config.ProgressDisplay, string, string, StreamWriterOptions) chatRenderer {
		built++
		return &recordingRenderer{}
	})
	sink.Register("sess", bgTarget{})

	// Two separate background turns (two subagent completions over time) must each
	// get their own renderer — the first is retired at its EventComplete.
	sink.Handle("sess", agent.Event{Type: agent.EventText, Text: "a"})
	sink.Handle("sess", agent.Event{Type: agent.EventComplete})
	sink.Handle("sess", agent.Event{Type: agent.EventText, Text: "b"})
	sink.Handle("sess", agent.Event{Type: agent.EventComplete})

	if built != 2 {
		t.Fatalf("expected a fresh renderer per background turn, got %d", built)
	}
}
