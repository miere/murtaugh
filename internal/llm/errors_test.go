package llm

import (
	"errors"
	"fmt"
	"testing"

	"github.com/voocel/litellm/providers"
)

// wrapLikeProduction reproduces the layers a provider error picks up on its way
// to the Slack renderer (llm.Stream → native.runTurn), so the tests prove
// Classify sees through the wrapping rather than through a lucky flat error.
func wrapLikeProduction(err error) error {
	return fmt.Errorf("native: provider stream: %w", fmt.Errorf("llm: gemini stream: %w", err))
}

// geminiOverloadBody is the verbatim payload behind the incident this classifier
// was written for.
const geminiOverloadBody = `{
  "error": {
    "code": 503,
    "message": "This model is currently experiencing high demand. Spikes in demand are usually temporary. Please try again later.",
    "status": "UNAVAILABLE"
  }
}`

func TestClassifyGeminiOverloaded(t *testing.T) {
	err := wrapLikeProduction(providers.NewHTTPError("gemini", 503, geminiOverloadBody))

	f, ok := Classify(err)
	if !ok {
		t.Fatalf("Classify() ok = false, want true")
	}
	if f.Kind != FailureOverloaded {
		t.Errorf("Kind = %q, want %q", f.Kind, FailureOverloaded)
	}
	if f.StatusCode != 503 {
		t.Errorf("StatusCode = %d, want 503", f.StatusCode)
	}
	if !f.Retryable {
		t.Error("Retryable = false, want true")
	}
	want := "This model is currently experiencing high demand. Spikes in demand are usually temporary. Please try again later."
	if f.Message != want {
		t.Errorf("Message = %q, want %q", f.Message, want)
	}
	if got := f.String(); got != "Gemini is overloaded (503)" {
		t.Errorf("String() = %q, want %q", got, "Gemini is overloaded (503)")
	}
}

func TestClassifyPerFamily(t *testing.T) {
	tests := []struct {
		name        string
		provider    string
		status      int
		body        string
		wantKind    FailureKind
		wantRetry   bool
		wantMessage string
		wantString  string
	}{
		{
			name:        "anthropic overloaded",
			provider:    "anthropic",
			status:      529,
			body:        `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			wantKind:    FailureOverloaded,
			wantRetry:   true,
			wantMessage: "Overloaded",
			wantString:  "Anthropic is overloaded (529)",
		},
		{
			name:        "openai bad credentials",
			provider:    "openai",
			status:      401,
			body:        `{"error":{"message":"Incorrect API key provided.","type":"invalid_request_error","code":"invalid_api_key"}}`,
			wantKind:    FailureAuth,
			wantRetry:   false,
			wantMessage: "Incorrect API key provided.",
			wantString:  "OpenAI rejected the credentials (401)",
		},
		{
			name:        "rate limited",
			provider:    "gemini",
			status:      429,
			body:        `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota)."}}`,
			wantKind:    FailureRateLimit,
			wantRetry:   true,
			wantMessage: "Resource has been exhausted (e.g. check quota).",
			wantString:  "Gemini is rate limiting us (429)",
		},
		{
			name:        "unknown model",
			provider:    "openai",
			status:      404,
			body:        `{"error":{"message":"The model 'gpt-9' does not exist"}}`,
			wantKind:    FailureModel,
			wantRetry:   false,
			wantMessage: "The model 'gpt-9' does not exist",
			wantString:  "OpenAI does not know this model (404)",
		},
		{
			name:        "context overflow",
			provider:    "anthropic",
			status:      400,
			body:        `{"error":{"message":"prompt is too long: 250000 tokens > 200000 maximum"}}`,
			wantKind:    FailureContextOverflow,
			wantRetry:   false,
			wantMessage: "prompt is too long: 250000 tokens > 200000 maximum",
			wantString:  "the conversation is too long for this model (400)",
		},
		{
			name:      "html body falls back to the whole message",
			provider:  "openai",
			status:    502,
			body:      "<html><body>502 Bad Gateway</body></html>",
			wantKind:  FailureProvider,
			wantRetry: true,
			// No JSON to dig into: the label litellm added plus the body, whitespace collapsed.
			wantMessage: "provider error (HTTP 502): <html><body>502 Bad Gateway</body></html>",
			wantString:  "OpenAI returned an error (502)",
		},
		{
			name:        "array-wrapped body",
			provider:    "gemini",
			status:      503,
			body:        `[{"error":{"code":503,"message":"The service is currently unavailable."}}]`,
			wantKind:    FailureOverloaded,
			wantRetry:   true,
			wantMessage: "The service is currently unavailable.",
			wantString:  "Gemini is overloaded (503)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, ok := Classify(wrapLikeProduction(providers.NewHTTPError(tc.provider, tc.status, tc.body)))
			if !ok {
				t.Fatalf("Classify() ok = false, want true")
			}
			if f.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", f.Kind, tc.wantKind)
			}
			if f.Retryable != tc.wantRetry {
				t.Errorf("Retryable = %v, want %v", f.Retryable, tc.wantRetry)
			}
			if f.Message != tc.wantMessage {
				t.Errorf("Message = %q, want %q", f.Message, tc.wantMessage)
			}
			if got := f.String(); got != tc.wantString {
				t.Errorf("String() = %q, want %q", got, tc.wantString)
			}
		})
	}
}

// TestClassifyNonProviderError pins the fallback contract: anything that did not
// come from the provider boundary is NOT classified, so the renderer keeps
// showing the raw error (which is the useful thing for our own bugs).
func TestClassifyNonProviderError(t *testing.T) {
	for _, err := range []error{
		nil,
		errors.New("acp: session terminated"),
		fmt.Errorf("native: stream event: %w", errors.New("read tcp: connection reset by peer")),
	} {
		if _, ok := Classify(err); ok {
			t.Errorf("Classify(%v) ok = true, want false", err)
		}
	}
}

// TestClassifyNetworkError covers the non-HTTP provider failure: no status code,
// so the label must not read "(0)".
func TestClassifyNetworkError(t *testing.T) {
	err := providers.NewNetworkError("gemini", "dial tcp: no such host", errors.New("dns"))

	f, ok := Classify(wrapLikeProduction(err))
	if !ok {
		t.Fatalf("Classify() ok = false, want true")
	}
	if f.Kind != FailureNetwork {
		t.Errorf("Kind = %q, want %q", f.Kind, FailureNetwork)
	}
	if got := f.String(); got != "Gemini is unreachable" {
		t.Errorf("String() = %q, want %q", got, "Gemini is unreachable")
	}
}

// TestHeadlineUnknownProvider covers a compat endpoint (Z.ai, DeepSeek…) and an
// absent provider name — neither may produce a headline with a hole in it.
func TestHeadlineUnknownProvider(t *testing.T) {
	if got := (Failure{Kind: FailureOverloaded, Provider: "z.ai"}).String(); got != "z.ai is overloaded" {
		t.Errorf("String() = %q, want %q", got, "z.ai is overloaded")
	}
	if got := (Failure{Kind: FailureTimeout}).String(); got != "the model provider timed out" {
		t.Errorf("String() = %q, want %q", got, "the model provider timed out")
	}
}
