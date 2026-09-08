package gateway

import (
	"errors"
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/providerfail"
	"github.com/miere/murtaugh/internal/slack/alertcard"
)

// geminiOverload is the error a native agent hands the gateway when its provider
// is overloaded: the original chain's text, carrying the classification the
// backend derived at the point of failure (internal/agent/native's eventError,
// via llm.CarryFailure — see TestEventErrorCarriesTheProviderClassification for
// the producing half). The gateway classifies nothing itself; it links no
// provider client and cannot.
func geminiOverload() error {
	return providerfail.New(providerfail.Failure{
		Kind:       providerfail.Overloaded,
		Provider:   "gemini",
		StatusCode: 503,
		Message:    "This model is currently experiencing high demand. Spikes in demand are usually temporary. Please try again later.",
		Retryable:  true,
	}, `native: provider stream: llm: gemini stream: provider error (HTTP 503): {"error":{"code":503,"message":"This model is currently experiencing high demand. Spikes in demand are usually temporary. Please try again later.","status":"UNAVAILABLE"}}`)
}

// TestFailSpecProviderFailure pins what the user actually reads when a native
// agent's provider is down — the incident this replaced was a three-layer Go
// error chain wrapped around a pretty-printed JSON body.
func TestFailSpecProviderFailure(t *testing.T) {
	spec := failSpec(geminiOverload())

	if spec.Level != alertcard.LevelError {
		t.Errorf("Level = %q, want error", spec.Level)
	}
	if spec.Subtitle != "The agent is not available." {
		t.Errorf("Subtitle = %q", spec.Subtitle)
	}
	if spec.Reason != "Gemini is overloaded (503)" {
		t.Errorf("Reason = %q, want the classified headline", spec.Reason)
	}
	if !strings.Contains(spec.Text, "experiencing high demand") {
		t.Errorf("Text = %q, want the provider's own sentence", spec.Text)
	}
	// The remedy follows from the kind: an overload is worth waiting out.
	if spec.NextSteps != "Try again in a moment." {
		t.Errorf("NextSteps = %q", spec.NextSteps)
	}
	if strings.Contains(spec.Subtitle, "ACP agent") {
		t.Error("provider failure must not be attributed to the ACP agent")
	}
}

// The collapsed card is what makes keeping the whole chain affordable: it costs
// no screen space until someone opens it, and it is exactly what diagnosing the
// failure needs.
func TestFailSpecKeepsTheUnabridgedError(t *testing.T) {
	err := geminiOverload()

	if got := failSpec(err).Detail; got != err.Error() {
		t.Errorf("Detail = %q, want the full error chain %q", got, err.Error())
	}
}

// Everything that is not a provider error keeps the generic headline with the
// raw error, which is what diagnosing an ACP or spawn fault needs.
func TestFailSpecNonProviderFailure(t *testing.T) {
	spec := failSpec(errors.New("acp: session terminated"))

	if spec.Subtitle != "Murtaugh hit an error while talking to the agent." {
		t.Errorf("Subtitle = %q, want the generic notice", spec.Subtitle)
	}
	if spec.Detail != "acp: session terminated" {
		t.Errorf("Detail = %q, want the raw error", spec.Detail)
	}
	if spec.Reason != "" {
		t.Errorf("Reason = %q, want none for an unclassified error", spec.Reason)
	}
	// No hand-written next steps, so the card supplies the level's own.
	if !strings.Contains(alertcard.PlainText(spec), "notify your admin user") {
		t.Errorf("rendered alert carries no guidance: %q", alertcard.PlainText(spec))
	}
}

// Fail(nil) still produces a usable alert rather than an empty card.
func TestFailSpecNilError(t *testing.T) {
	spec := failSpec(nil)

	if spec.Level != alertcard.LevelError {
		t.Errorf("Level = %q, want error", spec.Level)
	}
	if spec.Subtitle == "" {
		t.Error("a nil error must still say something")
	}
	if spec.Detail != "" {
		t.Errorf("Detail = %q, want empty for a nil error", spec.Detail)
	}
	if got := alertcard.PlainText(spec); strings.HasSuffix(got, "\n") {
		t.Errorf("PlainText = %q, want no trailing newline", got)
	}
}
