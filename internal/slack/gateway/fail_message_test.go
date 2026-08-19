package gateway

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/voocel/litellm/providers"
)

// TestStreamFailMessageProviderFailure pins the message the user actually reads
// when a native agent's provider is down — the incident this replaced was a
// three-layer Go error chain wrapped around a pretty-printed JSON body.
func TestStreamFailMessageProviderFailure(t *testing.T) {
	body := `{"error":{"code":503,"message":"This model is currently experiencing high demand. Spikes in demand are usually temporary. Please try again later.","status":"UNAVAILABLE"}}`
	err := fmt.Errorf("native: provider stream: %w",
		fmt.Errorf("llm: gemini stream: %w", providers.NewHTTPError("gemini", 503, body)))

	got := streamFailMessage(err)

	want := "\n\n:warning: *Agent is not available* — Gemini is overloaded (503)" +
		"\nThis model is currently experiencing high demand. Spikes in demand are usually temporary. Please try again later."
	if got != want {
		t.Errorf("streamFailMessage() =\n%q\nwant\n%q", got, want)
	}
	if strings.Contains(got, "ACP agent") {
		t.Error("provider failure must not be attributed to the ACP agent")
	}
}

// TestStreamFailMessageNonProviderFailure: everything that is not a provider
// error keeps the generic notice with the raw error, which is what diagnosing an
// ACP or spawn fault needs.
func TestStreamFailMessageNonProviderFailure(t *testing.T) {
	got := streamFailMessage(errors.New("acp: session terminated"))

	if !strings.Contains(got, "hit an error while talking to the agent") {
		t.Errorf("streamFailMessage() = %q, want the generic notice", got)
	}
	if !strings.Contains(got, "`acp: session terminated`") {
		t.Errorf("streamFailMessage() = %q, want the raw error in a code span", got)
	}
}

// TestStreamFailMessageNilError: Fail(nil) still paints a notice, never a bare
// warning with a dangling empty line.
func TestStreamFailMessageNilError(t *testing.T) {
	got := streamFailMessage(nil)

	if !strings.Contains(got, ":warning:") {
		t.Errorf("streamFailMessage(nil) = %q, want a warning notice", got)
	}
	if strings.HasSuffix(got, "\n") {
		t.Errorf("streamFailMessage(nil) = %q, want no trailing newline", got)
	}
}
