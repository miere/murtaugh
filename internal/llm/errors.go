package llm

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/voocel/litellm/providers"

	"github.com/miere/murtaugh/internal/providerfail"
)

// The failure vocabulary itself lives in internal/providerfail, a leaf that
// links no provider client, because the Slack gateway and the node protocol both
// paint these words and neither may reach litellm (#170 Change E). This package
// keeps the half that cannot leave: the mapping from litellm's concrete error
// onto that vocabulary.
//
// The aliases below are not a compatibility shim to be removed later — they are
// how a caller that already imports this package (it is making provider calls)
// spells the vocabulary without importing two packages to describe one failure.
type (
	// Failure is providerfail.Failure. See that package for the contract.
	Failure = providerfail.Failure
	// FailureKind is providerfail.Kind.
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

// NewFailureError is providerfail.New: an error carrying an already-derived
// classification, for the far side of a wire where the original error does not
// exist.
func NewFailureError(f Failure, text string) error { return providerfail.New(f, text) }

// Classify reduces any error returned by a Provider to a Failure. It reports
// false for anything that did not originate at the provider boundary (our own
// bugs, context cancellation, transport failures above this layer) — those carry
// no provider vocabulary and callers should fall back to a generic message.
//
// It matches on litellm's typed *providers.LiteLLMError via errors.As, so the
// fmt.Errorf wrapping every layer adds is transparent to it. It also matches an
// error that already carries a Failure (providerfail.Classify), so a provider
// failure classified once — on a runtime node, before serialisation — classifies
// identically wherever it is read.
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

// CarryFailure classifies err and, when it is a provider failure, returns err
// with that classification attached — same text, same unwrap chain, so nothing
// downstream that compares by identity is disturbed. An error that is not a
// provider failure, and a nil error, are returned unchanged.
//
// It is called at the seam where a provider error stops being litellm's and
// becomes the agent layer's, and it is what lets every reader past that seam —
// the Slack alert card, the wire encoder, a log — use providerfail.Classify
// without linking a provider client. Classifying at the source is also the only
// way the local and remote paths can agree: on the wire there is no
// *providers.LiteLLMError left to classify.
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
