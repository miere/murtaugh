package gateway

import (
	"context"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/slack/alertcard"
)

// recordingRenderer is a chatRenderer that records what it was driven with.
// Shared by the background-events router tests and the eventTranslator tests,
// which is why it records an ORDERED call log as well as counters: the whole
// claim of the inbound/outbound layering (#188) is about what the renderer is
// handed and in what order, and a counter cannot express "exactly one call, and
// it was Finish".
//
// It carries no clock, deliberately: it is a chatRenderer, so the renderclock
// guard (internal/archtest/renderclockanalyzer) covers it too.
type recordingRenderer struct {
	// calls names each method as it was driven, in order.
	calls []string
	// text accumulates every rendered chunk, so a test can assert the reply the
	// user would have seen rather than just how many writes happened.
	text          string
	tasks         int
	attachments   int
	interjections int
	finished      bool
	// empty is the alert Finish was handed: nil means the turn produced a reply.
	empty       *alertcard.Spec
	failed      error
	interrupted bool
	stopped     bool
	// textErr, when set, makes Text report a delivery failure — Slack refusing a
	// write, which is not the same thing as the agent failing.
	textErr error
}

func (r *recordingRenderer) Text(_ context.Context, text string) error {
	r.calls = append(r.calls, "Text")
	r.text += text
	return r.textErr
}

func (r *recordingRenderer) Task(context.Context, *agent.TaskEvent) error {
	r.calls = append(r.calls, "Task")
	r.tasks++
	return nil
}

func (r *recordingRenderer) Attachment(context.Context, *agent.AttachmentEvent) error {
	r.calls = append(r.calls, "Attachment")
	r.attachments++
	return nil
}

func (r *recordingRenderer) BeginInterjection(context.Context) {
	r.calls = append(r.calls, "BeginInterjection")
	r.interjections++
}

func (r *recordingRenderer) Finish(_ context.Context, empty *alertcard.Spec) error {
	r.calls = append(r.calls, "Finish")
	r.finished = true
	r.empty = empty
	return nil
}

func (r *recordingRenderer) Fail(_ context.Context, err error) error {
	r.calls = append(r.calls, "Fail")
	r.failed = err
	return nil
}

func (r *recordingRenderer) Interrupted(context.Context) {
	r.calls = append(r.calls, "Interrupted")
	r.interrupted = true
}

func (r *recordingRenderer) EnsureStopped(context.Context) {
	r.calls = append(r.calls, "EnsureStopped")
	r.stopped = true
}

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
