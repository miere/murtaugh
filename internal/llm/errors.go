package llm

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/voocel/litellm/providers"

	"github.com/miere/murtaugh/internal/providerfail"
)

// Aliases of internal/providerfail, which lets the gateway name failures without
// linking litellm; they stay so a caller making provider calls needs one import.
type (
	Failure     = providerfail.Failure
	FailureKind = providerfail.Kind
)

const (
	FailureAuth            = providerfail.Auth
	FailureRateLimit       = providerfail.RateLimit
	FailureOverloaded      = providerfail.Overloaded
	FailureQuota           = providerfail.Quota
	FailureContextOverflow = providerfail.ContextOverflow
	FailureModel           = providerfail.Model
	FailureValidation      = providerfail.Validation
	FailureNetwork         = providerfail.Network
	FailureTimeout         = providerfail.Timeout
	FailureProvider        = providerfail.Provider
)

// For the far side of a wire, where the original litellm error no longer exists.
func NewFailureError(f Failure, text string) error { return providerfail.New(f, text) }

// Classify reduces any error returned by a Provider to a Failure. It reports
// false for anything that did not originate at the provider boundary (our own
// bugs, context cancellation, transport failures above this layer) — those carry
// no provider vocabulary and callers should fall back to a generic message.
//
// It matches on litellm's typed *providers.LiteLLMError via errors.As, so the
// fmt.Errorf wrapping every layer adds is transparent to it.
func Classify(err error) (Failure, bool) {
	if f, ok := providerfail.Classify(err); ok {
		return f, true
	}
	var lerr *providers.LiteLLMError
	if !errors.As(err, &lerr) {
		return Failure{}, false
	}

	return Failure{
		Kind:       kindOf(lerr),
		Provider:   lerr.Provider,
		StatusCode: lerr.StatusCode,
		Message:    humanMessage(lerr.Message),
		Retryable:  lerr.Retryable,
	}, true
}

// Classify here, at the source: once the error has crossed the wire there is no
// litellm error left, so every reader downstream relies on what this attaches.
func CarryFailure(err error) error {
	f, ok := Classify(err)
	if !ok {
		return err
	}
	return providerfail.Carrying(f, err)
}

// kindOf maps litellm's ErrorType onto a FailureKind, with one refinement:
// litellm classifies every 5xx except 529 as a generic provider error, but a 503
// is specifically "no capacity right now" — the difference between "try again in
// a minute" and "something is broken upstream", which is exactly what the user
// needs to know.
func kindOf(e *providers.LiteLLMError) FailureKind {
	if e.Type == providers.ErrorTypeProvider && e.StatusCode == 503 {
		return FailureOverloaded
	}
	switch e.Type {
	case providers.ErrorTypeAuth:
		return FailureAuth
	case providers.ErrorTypeRateLimit:
		return FailureRateLimit
	case providers.ErrorTypeOverloaded:
		return FailureOverloaded
	case providers.ErrorTypeQuota:
		return FailureQuota
	case providers.ErrorTypeContextOverflow:
		return FailureContextOverflow
	case providers.ErrorTypeModel:
		return FailureModel
	case providers.ErrorTypeValidation:
		return FailureValidation
	case providers.ErrorTypeNetwork:
		return FailureNetwork
	case providers.ErrorTypeTimeout:
		return FailureTimeout
	default:
		return FailureProvider
	}
}

// humanMessage digs the provider's own sentence out of a litellm error message.
// litellm hands us the raw HTTP body prefixed with its own label, e.g.
//
//	provider error (HTTP 503): { "error": { "code": 503, "message": "This model
//	is currently experiencing high demand...", "status": "UNAVAILABLE" } }
//
// Gemini, OpenAI and Anthropic all put the human sentence at `error.message`, so
// one extraction covers every family we support. Anything that does not parse
// (a proxy's HTML error page, a plain-text body) falls back to the whole message
// with its whitespace collapsed — degraded, never empty.
func humanMessage(raw string) string {
	if m := extractJSONMessage(raw); m != "" {
		return collapseSpace(m)
	}
	return collapseSpace(raw)
}

// errorBody models the response shape shared by the providers we support. Both
// nestings are accepted: `{"error":{"message":...}}` and a bare
// `{"message":...}`.
type errorBody struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

func (b errorBody) message() string {
	if b.Error.Message != "" {
		return b.Error.Message
	}
	return b.Message
}

// extractJSONMessage finds the JSON payload embedded in raw and returns its
// error message, or "" when there is none to find. Gemini sometimes wraps the
// object in an array, so both are tried.
func extractJSONMessage(raw string) string {
	start := strings.IndexAny(raw, "{[")
	if start < 0 {
		return ""
	}
	payload := []byte(raw[start:])

	var obj errorBody
	if err := json.Unmarshal(payload, &obj); err == nil {
		return obj.message()
	}

	var arr []errorBody
	if err := json.Unmarshal(payload, &arr); err == nil {
		for _, b := range arr {
			if m := b.message(); m != "" {
				return m
			}
		}
	}
	return ""
}

// collapseSpace folds every whitespace run into a single space so a pretty-printed
// JSON body does not paint as a ragged multi-line block.
func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
