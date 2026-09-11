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

// The alert card and the wire encoder link no provider client, so the error must
// arrive classified or both silently fall back to the generic error.
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

	if got := ev.Error.Error(); got != raw.Error() {
		t.Errorf("Error() = %q, want the original chain %q", got, raw.Error())
	}
	var lerr *providers.LiteLLMError
	if !errors.As(ev.Error, &lerr) {
		t.Error("errors.As(ev.Error, **providers.LiteLLMError) = false; attaching the classification broke the unwrap chain")
	}
}

// The chat handler compares this error by identity, so a cancellation must stay
// the same error value.
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
