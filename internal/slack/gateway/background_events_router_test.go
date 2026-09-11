package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/slack/alertcard"
)

// recordingRenderer is a chatRenderer that records what it was driven with.
type recordingRenderer struct {
	calls         []string
	text          string
	tasks         int
	attachments   int
	interjections int
	finished      bool
	empty         *alertcard.Spec
	failed        error
	interrupted   bool
	stopped       bool
	textErr       error
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

type settlingRenderer struct {
	*recordingRenderer
	settled chan struct{}
}

func newSettlingRenderer() *settlingRenderer {
	return &settlingRenderer{recordingRenderer: &recordingRenderer{}, settled: make(chan struct{}, 4)}
}

func (r *settlingRenderer) EnsureStopped(ctx context.Context) {
	r.recordingRenderer.EnsureStopped(ctx)
	select {
	case r.settled <- struct{}{}:
	default:
	}
}

func awaitSettled(t *testing.T, r *settlingRenderer, why string) {
	t.Helper()
	select {
	case <-r.settled:
	case <-time.After(2 * time.Second):
		t.Fatalf("renderer was never finalised: %s", why)
	}
}

func TestBackgroundSinkRendersRegisteredSession(t *testing.T) {
	sink := newBackgroundEventsRouter(nil, 0)
	var made []*recordingRenderer
	sink.bind(func(string, string, StreamWriterOptions) chatRenderer {
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
	sink := newBackgroundEventsRouter(nil, 0)
	built := false
	sink.bind(func(string, string, StreamWriterOptions) chatRenderer {
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
	sink := newBackgroundEventsRouter(nil, 0)
	built := 0
	sink.bind(func(string, string, StreamWriterOptions) chatRenderer {
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

// Sealing a stalled stretch silently would look like a finished reply, and failing
// it would say the agent broke.
func TestBackgroundStretchThatGoesSilentEndsWithTheStallNotice(t *testing.T) {
	sink := newBackgroundEventsRouter(nil, 30*time.Millisecond)
	r := newSettlingRenderer()
	sink.bind(func(string, string, StreamWriterOptions) chatRenderer { return r })
	sink.Register("sess", bgTarget{channelID: "C", threadTS: "1"})

	sink.Handle("sess", agent.Event{Type: agent.EventText, Text: "half a thought"})

	awaitSettled(t, r, "a silent background stretch left its message open")
	if r.failed != nil {
		t.Errorf("a stretch that merely went quiet was failed as an agent error: %v", r.failed)
	}
	if r.empty == nil {
		t.Fatal("the message was sealed with nothing below it — indistinguishable from a completed one")
	}
	if *r.empty != backgroundStalledSpec() {
		t.Errorf("expiry sealed the message with %+v, want the stall notice", *r.empty)
	}
	if got := strings.Join(r.calls, ","); got != "Text,Finish,EnsureStopped" {
		t.Errorf("unexpected renderer calls: %s", got)
	}
}

// A recording renderer accepts any card, so only a real renderer shows the stall
// is not drawn as an agent error.
func TestBackgroundStallDoesNotRenderAsAnAgentError(t *testing.T) {
	stream, msgr, api := &fakeStreamAPI{}, &fakeStatusMessenger{}, &fakeAlertAPI{}
	sink := newBackgroundEventsRouter(discardLogger(), time.Hour)
	sink.bind(func(string, string, StreamWriterOptions) chatRenderer {
		return alertRenderer(stream, msgr, api)
	})
	sink.Register("sess", bgTarget{channelID: "C1", threadTS: "100.0"})
	sink.Handle("sess", agent.Event{Type: agent.EventText, Text: "half a thought"})

	sink.mu.Lock()
	s := sink.active["sess"]
	sink.mu.Unlock()
	if s == nil {
		t.Fatal("no stretch was opened")
	}
	sink.expire("sess", s)

	if len(api.posts) != 1 {
		t.Fatalf("the stall notice did not reach the user: %d cards posted", len(api.posts))
	}
	blocks := string(api.posts[0].Blocks)
	if !strings.Contains(blocks, "went quiet and never finished") {
		t.Errorf("the card does not say what happened:\n%s", blocks)
	}
	if strings.Contains(blocks, "notify your admin user") {
		t.Errorf("a stretch that went quiet told the user to notify their admin:\n%s", blocks)
	}
	if strings.Contains(blocks, "hit an error while talking to the agent") {
		t.Errorf("a stretch that went quiet is reported as an agent error:\n%s", blocks)
	}
	if strings.Contains(blocks, "close-button-web") {
		t.Errorf("the stall notice is wearing the error icon:\n%s", blocks)
	}
	if !strings.Contains(blocks, "danger-electrician") {
		t.Errorf("the stall notice is not wearing the warn icon:\n%s", blocks)
	}
}

// The window measures idle gaps, not total runtime, so a stretch that keeps
// talking must outlive it.
func TestBackgroundStretchSurvivesEventsSpacedInsideTheWindow(t *testing.T) {
	const window = 100 * time.Millisecond
	sink := newBackgroundEventsRouter(nil, window)
	r := newSettlingRenderer()
	sink.bind(func(string, string, StreamWriterOptions) chatRenderer { return r })
	sink.Register("sess", bgTarget{channelID: "C", threadTS: "1"})

	started := time.Now()
	for i := 0; i < 10; i++ {
		sink.Handle("sess", agent.Event{Type: agent.EventText, Text: "."})
		time.Sleep(15 * time.Millisecond)
	}
	if elapsed := time.Since(started); elapsed <= window {
		t.Fatalf("the run took %s, which is inside the %s window — this test proves nothing", elapsed, window)
	}
	select {
	case <-r.settled:
		t.Fatal("a stretch that was still producing output was closed out")
	default:
	}

	sink.Handle("sess", agent.Event{Type: agent.EventComplete})
	awaitSettled(t, r, "the completion did not finalise the message")
	if r.failed != nil {
		t.Errorf("healthy stretch was failed: %v", r.failed)
	}
	if terminals := strings.Count(strings.Join(r.calls, ","), "EnsureStopped"); terminals != 1 {
		t.Errorf("expected exactly one terminal, got %d: %s", terminals, strings.Join(r.calls, ","))
	}
}

// A stretch left in the active map would swallow every later background event
// for that conversation.
func TestBackgroundExpiryRetiresTheStretch(t *testing.T) {
	sink := newBackgroundEventsRouter(nil, 20*time.Millisecond)
	var made []*settlingRenderer
	sink.bind(func(string, string, StreamWriterOptions) chatRenderer {
		r := newSettlingRenderer()
		made = append(made, r)
		return r
	})
	sink.Register("sess", bgTarget{channelID: "C", threadTS: "1"})

	sink.Handle("sess", agent.Event{Type: agent.EventText, Text: "a"})
	awaitSettled(t, made[0], "the stretch never expired")

	sink.Handle("sess", agent.Event{Type: agent.EventText, Text: "b"})
	if len(made) != 2 {
		t.Fatalf("expected a fresh renderer after an expiry, got %d — the expired stretch is still the session's", len(made))
	}
	if made[1].text != "b" {
		t.Errorf("the new stretch rendered %q, want %q", made[1].text, "b")
	}
	if got := strings.Join(made[0].calls, ","); got != "Text,Finish,EnsureStopped" {
		t.Errorf("the expired message was written to again: %s", got)
	}
}

// chatRenderer is not safe to drive twice, and expiry can race a terminal event.
// The race is driven directly because a test that must lose a race passes by luck.
func TestBackgroundStretchSettlesExactlyOnce(t *testing.T) {
	r := &recordingRenderer{}
	s := &bgStretch{renderer: r, window: time.Minute, idle: time.NewTimer(time.Minute), done: make(chan struct{})}

	if !s.end(func(r chatRenderer) { _ = r.Finish(context.Background(), nil) }) {
		t.Fatal("the first terminal was refused")
	}
	stalled := backgroundStalledSpec()
	if s.end(func(r chatRenderer) { _ = r.Finish(context.Background(), &stalled) }) {
		t.Error("a second terminal was allowed to settle the same stretch")
	}
	if s.render(func(r chatRenderer) { _ = r.Text(context.Background(), "late") }) {
		t.Error("an event was rendered into a stretch that had already been settled")
	}
	if got := strings.Join(r.calls, ","); got != "Finish" {
		t.Errorf("unexpected renderer calls: %s", got)
	}
	select {
	case <-s.done:
	default:
		t.Error("settling the stretch did not release its watcher")
	}
}

func TestBackgroundRouterArmsEachStretchWithTheConfiguredWindow(t *testing.T) {
	if got := newBackgroundEventsRouter(nil, 0).window; got != defaultBackgroundIdleTimeout {
		t.Errorf("non-positive window = %s, want the %s default", got, defaultBackgroundIdleTimeout)
	}
	if got := newBackgroundEventsRouter(nil, 42*time.Second).window; got != 42*time.Second {
		t.Errorf("configured window = %s, want 42s", got)
	}

	sink := newBackgroundEventsRouter(nil, 42*time.Second)
	sink.bind(func(string, string, StreamWriterOptions) chatRenderer {
		return &recordingRenderer{}
	})
	sink.Register("sess", bgTarget{})
	sink.Handle("sess", agent.Event{Type: agent.EventText, Text: "a"})
	s := sink.take("sess")
	if s == nil {
		t.Fatal("no stretch was opened")
	}
	if s.window != 42*time.Second {
		t.Errorf("stretch window = %s, want 42s", s.window)
	}
	s.end(func(chatRenderer) {})
}
