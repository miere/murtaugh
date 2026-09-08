package native

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/voocel/litellm/providers"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/providerfail"
)

// The terminal error event is where a provider failure stops being litellm's and
// becomes the agent layer's. It has to arrive already classified, because the two
// readers downstream — the Slack alert card and the wire encoder — are both in
// packages that link no provider client and so cannot classify it themselves.
//
// Without this the card degrades to the generic "Murtaugh hit an error" and the
// wire drops the provider discriminant, in both cases silently.
func TestEventErrorCarriesTheProviderClassification(t *testing.T) {
	body := `{"error":{"code":503,"message":"This model is currently experiencing high demand. Spikes in demand are usually temporary. Please try again later.","status":"UNAVAILABLE"}}`
	raw := fmt.Errorf("native: provider stream: %w",
		fmt.Errorf("llm: gemini stream: %w", providers.NewHTTPError("gemini", 503, body)))

	ev := eventError(raw)
	if ev.Type != agent.EventError {
		t.Fatalf("Type = %q, want %q", ev.Type, agent.EventError)
	}

	failure, ok := providerfail.Classify(ev.Error)
	if !ok {
		t.Fatal("providerfail.Classify(ev.Error) ok = false; the alert card would fall back to the generic error")
	}
	if got, want := failure.String(), "Gemini is overloaded (503)"; got != want {
		t.Errorf("Failure.String() = %q, want %q", got, want)
	}
	if failure.Kind != providerfail.Overloaded || !failure.Retryable {
		t.Errorf("Failure = %+v, want an overloaded, retryable failure", failure)
	}
	if !strings.Contains(failure.Message, "experiencing high demand") {
		t.Errorf("Failure.Message = %q, want the provider's own sentence", failure.Message)
	}

	// Attaching the classification must not rewrite what the error says: the card
	// puts this text in Detail verbatim and the journal records it as the turn's
	// error.
	if got := ev.Error.Error(); got != raw.Error() {
		t.Errorf("Error() = %q, want the original chain %q", got, raw.Error())
	}
	// Nor may it hide the chain: a reader further down that compares by identity
	// must still see through to the original.
	var lerr *providers.LiteLLMError
	if !errors.As(ev.Error, &lerr) {
		t.Error("errors.As(ev.Error, **providers.LiteLLMError) = false; attaching the classification broke the unwrap chain")
	}
}

// An error that is not a provider failure passes through untouched, so a
// cancellation is still a cancellation and the identity comparisons the chat
// handler makes on this event keep working.
func TestEventErrorLeavesNonProviderErrorsAlone(t *testing.T) {
	for _, raw := range []error{
		fmt.Errorf("native: turn interrupted: %w", context.Canceled),
		fmt.Errorf("native: %w: %q ran for 5m0s", agent.ErrToolCeiling, "terminal"),
		errors.New("native: tool registry is empty"),
	} {
		ev := eventError(raw)
		if ev.Error != raw {
			t.Errorf("eventError(%v).Error = %v, want the identical error value", raw, ev.Error)
		}
		if _, ok := providerfail.Classify(ev.Error); ok {
			t.Errorf("providerfail.Classify(%v) ok = true; a non-provider error must not acquire provider vocabulary", raw)
		}
	}
}
