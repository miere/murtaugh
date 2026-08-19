package native

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/llm"
	"github.com/voocel/litellm/providers"
)

// failingProvider fails the first failures Stream calls with err, then answers
// with a clean one-line end_turn. It counts calls so a test can assert how many
// attempts the loop actually made.
type failingProvider struct {
	err      error
	failures int
	calls    int
}

func (p *failingProvider) Stream(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
	p.calls++
	if p.calls <= p.failures {
		return nil, p.err
	}
	ch := make(chan llm.StreamEvent, 2)
	ch <- llm.StreamEvent{TextDelta: "recovered"}
	ch <- llm.StreamEvent{Done: true, StopReason: "end_turn", Usage: &llm.Usage{}}
	close(ch)
	return ch, nil
}

// shrinkRetryBackoff keeps the retry tests fast while preserving the retry COUNT
// (two waits = two retries), which is the part under test.
func shrinkRetryBackoff(t *testing.T) {
	t.Helper()
	original := providerRetryBackoff
	providerRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { providerRetryBackoff = original })
}

// overloadErr is a 503 wrapped the way llm.Stream wraps it in production.
func overloadErr() error {
	return errors.New("llm: gemini stream: " +
		providers.NewHTTPError("gemini", 503, `{"error":{"code":503,"message":"high demand"}}`).Error())
}

// retryableErr is the real typed error (not a flattened string), which is what
// the loop must classify.
func retryableErr() error {
	return providers.NewHTTPError("gemini", 503, `{"error":{"code":503,"message":"high demand"}}`)
}

// TestOpenStreamRetriesTransientFailure: a provider that 503s twice and then
// answers must produce a normal turn, not an error.
func TestOpenStreamRetriesTransientFailure(t *testing.T) {
	shrinkRetryBackoff(t)

	p := &failingProvider{err: retryableErr(), failures: 2}
	loop := NewLoop(p, "gemini-2.5-pro", nil, 4)
	conv := NewConversation()
	conv.AppendUser("hi")
	emit, evs := newCollector()

	stop, err := loop.Run(context.Background(), conv, "system", emit)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	if stop != "end_turn" {
		t.Errorf("stop reason = %q, want %q", stop, "end_turn")
	}
	if p.calls != 3 {
		t.Errorf("Stream calls = %d, want 3 (two failures + one success)", p.calls)
	}

	var retryNotices int
	for _, e := range eventsOfType(*evs, agent.EventStatus) {
		if strings.Contains(e.Text, "retrying") {
			retryNotices++
			if !strings.Contains(e.Text, "Gemini is overloaded (503)") {
				t.Errorf("retry status = %q, want it to name the failure", e.Text)
			}
		}
	}
	if retryNotices != 2 {
		t.Errorf("retry status events = %d, want 2", retryNotices)
	}
}

// TestOpenStreamGivesUpAfterBackoff: a provider that never recovers fails the
// turn once the backoff schedule is exhausted, rather than retrying forever.
func TestOpenStreamGivesUpAfterBackoff(t *testing.T) {
	shrinkRetryBackoff(t)

	p := &failingProvider{err: retryableErr(), failures: 99}
	loop := NewLoop(p, "gemini-2.5-pro", nil, 4)
	conv := NewConversation()
	conv.AppendUser("hi")
	emit, _ := newCollector()

	if _, err := loop.Run(context.Background(), conv, "system", emit); err == nil {
		t.Fatal("Run() error = nil, want the provider failure")
	}
	if want := 1 + len(providerRetryBackoff); p.calls != want {
		t.Errorf("Stream calls = %d, want %d (initial + one per backoff step)", p.calls, want)
	}
}

// TestOpenStreamDoesNotRetryPermanentFailure: a rejected credential will be
// rejected again — retrying only delays the report.
func TestOpenStreamDoesNotRetryPermanentFailure(t *testing.T) {
	shrinkRetryBackoff(t)

	p := &failingProvider{
		err:      providers.NewHTTPError("openai", 401, `{"error":{"message":"Incorrect API key provided."}}`),
		failures: 99,
	}
	loop := NewLoop(p, "gpt-5", nil, 4)
	conv := NewConversation()
	conv.AppendUser("hi")
	emit, _ := newCollector()

	if _, err := loop.Run(context.Background(), conv, "system", emit); err == nil {
		t.Fatal("Run() error = nil, want the auth failure")
	}
	if p.calls != 1 {
		t.Errorf("Stream calls = %d, want 1 (no retry on a permanent failure)", p.calls)
	}
}

// TestOpenStreamDoesNotRetryUnclassifiedFailure: an error from outside the
// provider boundary carries no retry hint, so it must surface immediately.
func TestOpenStreamDoesNotRetryUnclassifiedFailure(t *testing.T) {
	shrinkRetryBackoff(t)

	p := &failingProvider{err: overloadErr(), failures: 99} // stringified: not typed
	loop := NewLoop(p, "gemini-2.5-pro", nil, 4)
	conv := NewConversation()
	conv.AppendUser("hi")
	emit, _ := newCollector()

	if _, err := loop.Run(context.Background(), conv, "system", emit); err == nil {
		t.Fatal("Run() error = nil, want the provider failure")
	}
	if p.calls != 1 {
		t.Errorf("Stream calls = %d, want 1 (an unclassified error is not retried)", p.calls)
	}
}
