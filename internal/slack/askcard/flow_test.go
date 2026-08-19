package askcard

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/assets"
	slacklib "github.com/miere/murtaugh/internal/slack/client"
)

// syncAPI is a concurrency-safe SlackAPI fake: Ask posts and updates from the
// goroutine under test while the test clicks from another.
type syncAPI struct {
	slacklib.SlackAPI // unused methods panic if ever called

	mu      sync.Mutex
	posts   []slacklib.PostMessageParams
	updates []slacklib.UpdateMessageParams
	postCh  chan struct{}
	postErr error
}

func newSyncAPI() *syncAPI { return &syncAPI{postCh: make(chan struct{}, 16)} }

func (a *syncAPI) PostMessage(_ context.Context, p slacklib.PostMessageParams) (slacklib.PostMessageResult, error) {
	a.mu.Lock()
	if a.postErr != nil {
		a.mu.Unlock()
		return slacklib.PostMessageResult{}, a.postErr
	}
	a.posts = append(a.posts, p)
	ts := fmt.Sprintf("ts-%d", len(a.posts))
	a.mu.Unlock()
	select {
	case a.postCh <- struct{}{}:
	default:
	}
	return slacklib.PostMessageResult{Channel: p.ChannelID, TS: ts}, nil
}

func (a *syncAPI) UpdateMessage(_ context.Context, p slacklib.UpdateMessageParams) (slacklib.PostMessageResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.updates = append(a.updates, p)
	return slacklib.PostMessageResult{Channel: p.ChannelID, TS: p.TS}, nil
}

func (a *syncAPI) awaitPosts(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		a.mu.Lock()
		got := len(a.posts)
		a.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-a.postCh:
		case <-deadline:
			t.Fatalf("timed out waiting for %d posts; saw %d", n, got)
		}
	}
}

func (a *syncAPI) lastUpdate(t *testing.T) slacklib.UpdateMessageParams {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.updates) == 0 {
		t.Fatal("expected at least one card update")
	}
	return a.updates[len(a.updates)-1]
}

func (a *syncAPI) updateCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.updates)
}

func newTestFlow(api *syncAPI) *Flow {
	return New(
		slacklib.NewLazyClientWith(func() (slacklib.SlackAPI, error) { return api, nil }),
		NewRenderer("", assets.FS),
	)
}

// theOnlyCorrelation pulls the correlation id back out of the posted card, which
// is how the gateway would learn it (from the clicked action_id).
func theOnlyCorrelation(t *testing.T, f *Flow) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		f.mu.Lock()
		for corr := range f.sessions {
			f.mu.Unlock()
			return corr
		}
		f.mu.Unlock()
		select {
		case <-time.After(5 * time.Millisecond):
		case <-deadline:
			t.Fatal("no session registered")
		}
	}
}

// askInBackground starts Ask and returns a channel carrying its result.
func askInBackground(t *testing.T, f *Flow, spec Spec) <-chan Response {
	t.Helper()
	out := make(chan Response, 1)
	go func() {
		resp, err := f.Ask(context.Background(), Destination{ChannelID: "C1", ThreadTS: "1.0"}, spec)
		if err != nil {
			t.Errorf("Ask: %v", err)
		}
		out <- resp
	}()
	return out
}

func TestSubmitResolvesWithAnswers(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	done := askInBackground(t, f, twoQuestions())
	api.awaitPosts(t, 1)
	corr := theOnlyCorrelation(t, f)

	answers := map[string][]string{"q0": {"Redis"}, "q1": {"Hard delete"}}
	if err := f.HandleClick(context.Background(), corr, ActionSubmit, "U7", answers); err != nil {
		t.Fatalf("HandleClick: %v", err)
	}

	resp := <-done
	if !resp.Answered() {
		t.Fatalf("resp = %+v, want an answered response", resp)
	}
	if resp.UserID != "U7" {
		t.Errorf("UserID = %q, want U7", resp.UserID)
	}
	if got := resp.Answers["q0"]; len(got) != 1 || got[0] != "Redis" {
		t.Errorf("q0 = %v", got)
	}
	// The card settles to the answered state, with no live controls left.
	assertTerminalCard(t, api.lastUpdate(t))
}

func TestChatResolvesAsAConversationRequest(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	done := askInBackground(t, f, twoQuestions())
	api.awaitPosts(t, 1)
	corr := theOnlyCorrelation(t, f)

	if err := f.HandleClick(context.Background(), corr, ActionChat, "U7", nil); err != nil {
		t.Fatalf("HandleClick: %v", err)
	}

	resp := <-done
	if !resp.Chat {
		t.Fatalf("resp = %+v, want Chat", resp)
	}
	if resp.Answered() {
		t.Error("a chat request must not report as answered")
	}
	assertTerminalCard(t, api.lastUpdate(t))
}

// The heart of the validation path: Slack fires Submit regardless of what is
// filled in, so an incomplete form must re-prompt rather than resolve.
func TestIncompleteSubmitRepromptsWithoutResolving(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	done := askInBackground(t, f, twoQuestions())
	api.awaitPosts(t, 1)
	corr := theOnlyCorrelation(t, f)

	partial := map[string][]string{"q1": {"Hard delete"}}
	if err := f.HandleClick(context.Background(), corr, ActionSubmit, "U7", partial); err != nil {
		t.Fatalf("HandleClick: %v", err)
	}

	select {
	case resp := <-done:
		t.Fatalf("an incomplete submit resolved the ask: %+v", resp)
	case <-time.After(100 * time.Millisecond):
	}

	// The re-render carries the callout and keeps the answer already given.
	update := api.lastUpdate(t)
	var doc map[string]any
	if err := json.Unmarshal(update.Blocks, &doc); err != nil {
		t.Fatalf("re-render is not valid JSON: %v", err)
	}
	children := childBlocks(t, doc)
	if len(blocksOfType(children, "callout")) != 1 {
		t.Error("the re-prompt has no validation callout")
	}
	if len(blocksOfType(children, "actions")) != 1 {
		t.Error("the re-prompt lost its buttons, so the user cannot answer")
	}
	inputs := blocksOfType(children, "input")
	if _, ok := inputs[1]["element"].(map[string]any)["initial_options"]; !ok {
		t.Error("the re-prompt discarded the answer the user had already given")
	}

	// Completing it now resolves normally.
	full := map[string][]string{"q0": {"Redis"}, "q1": {"Hard delete"}}
	if err := f.HandleClick(context.Background(), corr, ActionSubmit, "U7", full); err != nil {
		t.Fatalf("second HandleClick: %v", err)
	}
	if resp := <-done; !resp.Answered() {
		t.Fatalf("resp = %+v, want answered", resp)
	}
}

// A second click on an already-resolved card must not panic or double-deliver.
func TestSecondClickIsDropped(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	done := askInBackground(t, f, twoQuestions())
	api.awaitPosts(t, 1)
	corr := theOnlyCorrelation(t, f)

	answers := map[string][]string{"q0": {"Redis"}, "q1": {"Hard delete"}}
	if err := f.HandleClick(context.Background(), corr, ActionSubmit, "U7", answers); err != nil {
		t.Fatalf("first click: %v", err)
	}
	<-done
	// The session is unregistered once Ask returns, so a late click is simply
	// reported as unknown rather than resolving anything.
	if err := f.HandleClick(context.Background(), corr, ActionSubmit, "U8", answers); err == nil {
		t.Error("a click on a settled card should report no waiting question")
	}
}

func TestTimeoutSettlesTheCard(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	spec := twoQuestions()
	spec.Timeout = 20 * time.Millisecond

	resp, err := f.Ask(context.Background(), Destination{ChannelID: "C1"}, spec)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !resp.TimedOut {
		t.Fatalf("resp = %+v, want TimedOut", resp)
	}
	assertTerminalCard(t, api.lastUpdate(t))
}

func TestCancelledTurnSettlesTheCard(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	resp, err := f.Ask(ctx, Destination{ChannelID: "C1"}, twoQuestions())
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !resp.Cancelled {
		t.Fatalf("resp = %+v, want Cancelled", resp)
	}
	// settle runs on a fresh context, so the card is still rewritten even though
	// the turn's context is already dead.
	if api.updateCount() == 0 {
		t.Error("a cancelled ask left its card showing live inputs")
	}
}

func TestAskRejectsUnusableSpecs(t *testing.T) {
	f := newTestFlow(newSyncAPI())
	if _, err := f.Ask(context.Background(), Destination{}, twoQuestions()); err == nil {
		t.Error("expected an error with no channel")
	}
	if _, err := f.Ask(context.Background(), Destination{ChannelID: "C1"}, Spec{}); err == nil {
		t.Error("expected an error with no questions")
	}
	noOptions := Spec{Questions: []Question{{Question: "what?"}}}
	if _, err := f.Ask(context.Background(), Destination{ChannelID: "C1"}, noOptions); err == nil {
		t.Error("expected an error for a question with no options")
	}
}

// Keys are assigned in order when the caller leaves them blank, which is what
// makes the validation message's question numbers correct.
func TestAskAssignsMissingKeys(t *testing.T) {
	api := newSyncAPI()
	f := newTestFlow(api)
	spec := Spec{
		Timeout: 20 * time.Millisecond,
		Questions: []Question{
			{Question: "first", Options: []Option{{Label: "a"}, {Label: "b"}}},
			{Question: "second", Options: []Option{{Label: "c"}, {Label: "d"}}},
		},
	}
	if _, err := f.Ask(context.Background(), Destination{ChannelID: "C1"}, spec); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	var doc map[string]any
	api.mu.Lock()
	raw := api.posts[0].Blocks
	api.mu.Unlock()
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("posted card is not valid JSON: %v", err)
	}
	inputs := blocksOfType(childBlocks(t, doc), "input")
	for i, want := range []string{inputPrefix + "q0", inputPrefix + "q1"} {
		if got := inputs[i]["block_id"]; got != want {
			t.Errorf("input %d block_id = %v, want %v", i, got, want)
		}
	}
}

// assertTerminalCard checks a settled card has no way left to interact with it.
func assertTerminalCard(t *testing.T, update slacklib.UpdateMessageParams) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(update.Blocks, &doc); err != nil {
		t.Fatalf("terminal card is not valid JSON: %v\n%s", err, update.Blocks)
	}
	children := childBlocks(t, doc)
	if n := len(blocksOfType(children, "input")); n != 0 {
		t.Errorf("terminal card still has %d input blocks", n)
	}
	if n := len(blocksOfType(children, "actions")); n != 0 {
		t.Errorf("terminal card still has %d actions blocks", n)
	}
	if strings.TrimSpace(update.Text) == "" {
		t.Error("terminal card has no fallback text")
	}
}
