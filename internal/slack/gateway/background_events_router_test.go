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
	// empty is the alert Finish was handed — the empty-reply card, or a
	// background stretch's stall notice. nil means the turn ended with nothing
	// extra to say.
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

// settlingRenderer is a recordingRenderer that announces every terminal on a
// channel. The liveness tests need it for two reasons: an expiry runs on the
// router's watcher goroutine, so a test has to WAIT for it rather than sleep and
// hope; and recordingRenderer keeps its ordered call log in a plain slice, so
// receiving from the channel is also the happens-before edge that makes reading
// that log from the test goroutine safe.
type settlingRenderer struct {
	*recordingRenderer
	// settled takes one token per EnsureStopped — the call every terminal path
	// ends with. Buffered and sent to without blocking, because the send happens
	// while the stretch lock is held.
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

// awaitSettled waits for r to be finalised, failing the test rather than hanging
// if it never is — "the message was left open" is the bug under test, so it must
// surface as a failure and not a timeout.
//
// A free function, not a method: settlingRenderer is a chatRenderer, and the
// renderclock guard forbids a clock on one. The deadline here is a test's
// patience, not a renderer observing time — but the guard follows receivers and
// cannot tell the difference, and keeping it blunt is the point of it.
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

// A background stretch that emits text and then never emits a terminal event is
// the bug #192 names: nothing else in the process can close that Slack message.
// This asserts the whole terminal — Finish carrying the stall notice, then
// EnsureStopped — because sealing it silently would leave the user a message
// indistinguishable from one that completed, and failing it would tell them the
// agent broke.
//
// What that notice actually RENDERS as is asserted separately, against a real
// renderer, in TestBackgroundStallDoesNotRenderAsAnAgentError. Neither test is
// sufficient alone: this one cannot see a card, and a recordingRenderer will
// hold any spec at all without complaint.
func TestBackgroundStretchThatGoesSilentEndsWithTheStallNotice(t *testing.T) {
	sink := newBackgroundEventsRouter(nil, 30*time.Millisecond)
	r := newSettlingRenderer()
	sink.bind(func(string, string, StreamWriterOptions) chatRenderer { return r })
	sink.Register("sess", bgTarget{channelID: "C", threadTS: "1"})

	sink.Handle("sess", agent.Event{Type: agent.EventText, Text: "half a thought"})
	// No EventComplete, no EventError — the stream simply stops.

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

// The claim the recording renderer above cannot make: what the user READS.
//
// The router is bound to a real sectionRenderer with a real alert-card renderer
// behind it — the same pair production wires — and expire is called on this
// goroutine rather than waited for, so the assertions below are about a card
// that was genuinely rendered and posted.
//
// It exists because the previous version of this path called Fail, which routes
// through failSpec: an `error`-level card headed "Murtaugh hit an error while
// talking to the agent" and closing with "notify your admin user", for a stretch
// that had merely gone quiet. Nothing caught that, because the only test looked
// at the raw error rather than at the render.
func TestBackgroundStallDoesNotRenderAsAnAgentError(t *testing.T) {
	stream, msgr, api := &fakeStreamAPI{}, &fakeStatusMessenger{}, &fakeAlertAPI{}
	// A window long enough that the watcher goroutine cannot reach the renderer:
	// this test drives the expiry itself, so there is no race to lose.
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
	// Icons are how the level reaches the eye: the warn icon, not the error one.
	if strings.Contains(blocks, "close-button-web") {
		t.Errorf("the stall notice is wearing the error icon:\n%s", blocks)
	}
	if !strings.Contains(blocks, "danger-electrician") {
		t.Errorf("the stall notice is not wearing the warn icon:\n%s", blocks)
	}
}

// The counterpart, and the one that keeps the detector honest: a stretch that
// keeps producing output for longer than a whole window is NOT killed. The events
// here span more than the window with every gap far inside it, which is exactly
// the shape "size it against idle gaps, not against runtime" has to survive.
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

// Expiry has to RETIRE the stretch, not just paint it. If the entry stayed in the
// active map, every later background event for that conversation would be handed
// a sealed message and dropped — the leak the timer exists to prevent, made
// permanent. The conversation's TARGET, by contrast, must survive: the thread is
// still there.
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

	// The next background event for the same conversation, with no terminal in
	// between: it must open a fresh message in the same thread.
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

// The expiry and a terminal event can reach the same stretch at once — the
// watcher wakes up as the completion arrives — and chatRenderer is not safe to
// drive twice or concurrently. This pins the guard that decides the race:
// whichever gets there first settles the message, the other is told it lost and
// does not touch the renderer at all. Driven directly rather than through a real race,
// because a test that has to lose a race to fail is a test that passes by luck.
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

	// And the configured window is what a stretch is actually armed with — a knob
	// the constructor keeps to itself would be no knob at all.
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
	// take alone does not release the watcher; end does.
	s.end(func(chatRenderer) {})
}
