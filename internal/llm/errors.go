package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/voocel/litellm/providers"
)

// FailureKind is the coarse reason a provider call failed. It is the vocabulary
// the UI reasons about: every kind answers "is this the user's problem, ours, or
// the provider's, and is retrying worth anything?".
type FailureKind string

const (
	// FailureAuth — the credential was rejected (401/403). Ours to fix.
	FailureAuth FailureKind = "auth"
	// FailureRateLimit — we are sending too fast (429). Transient.
	FailureRateLimit FailureKind = "rate_limit"
	// FailureOverloaded — the provider is out of capacity (503/529). Transient
	// and not our fault; the canonical "try again in a minute" case.
	FailureOverloaded FailureKind = "overloaded"
	// FailureQuota — billing/quota exhausted (402). Ours to fix, not transient.
	FailureQuota FailureKind = "quota"
	// FailureContextOverflow — the conversation exceeds the model's window.
	FailureContextOverflow FailureKind = "context_overflow"
	// FailureModel — the model name is unknown to the provider (404).
	FailureModel FailureKind = "model"
	// FailureValidation — the provider rejected the request shape (400).
	FailureValidation FailureKind = "validation"
	// FailureNetwork — we could not reach the provider at all.
	FailureNetwork FailureKind = "network"
	// FailureTimeout — the provider did not answer in time.
	FailureTimeout FailureKind = "timeout"
	// FailureProvider — a server-side error that is none of the above (5xx).
	FailureProvider FailureKind = "provider"
)

// Failure is a provider error reduced to what a caller can act on: what went
// wrong (Kind), who said so (Provider/StatusCode), the provider's own sentence
// for a human (Message), and whether retrying the identical request could
// plausibly succeed (Retryable).
//
// It exists so callers — the agent loop deciding whether to retry, the Slack
// renderer deciding what to paint — never sniff error strings. Classify does the
// one string-shaped job (digging the sentence out of a JSON body) exactly once.
type Failure struct {
	Kind       FailureKind
	Provider   string
	StatusCode int
	Message    string
	Retryable  bool
}

// Classify reduces any error returned by a Provider to a Failure. It reports
// false for anything that did not originate at the provider boundary (our own
// bugs, context cancellation, transport failures above this layer) — those carry
// no provider vocabulary and callers should fall back to a generic message.
//
// It matches on litellm's typed *providers.LiteLLMError via errors.As, so the
// fmt.Errorf wrapping every layer adds is transparent to it.
func Classify(err error) (Failure, bool) {
	if err == nil {
		return Failure{}, false
	}
	var lerr *providers.LiteLLMError
	if !errors.As(err, &lerr) {
		return Failure{}, false
	}

	f := Failure{
		Kind:       kindOf(lerr),
		Provider:   lerr.Provider,
		StatusCode: lerr.StatusCode,
		Message:    humanMessage(lerr.Message),
		Retryable:  lerr.Retryable,
	}
	return f, true
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

// Headline is a one-line human summary naming the provider and what it did —
// "Gemini is overloaded", "OpenAI rejected the credentials". It carries no
// markup: the transport (Slack, a log line) decorates it.
func (f Failure) Headline() string {
	who := f.providerLabel()
	switch f.Kind {
	case FailureAuth:
		return who + " rejected the credentials"
	case FailureRateLimit:
		return who + " is rate limiting us"
	case FailureOverloaded:
		return who + " is overloaded"
	case FailureQuota:
		return "the " + who + " account is out of quota"
	case FailureContextOverflow:
		return "the conversation is too long for this model"
	case FailureModel:
		return who + " does not know this model"
	case FailureValidation:
		return who + " rejected the request"
	case FailureNetwork:
		return who + " is unreachable"
	case FailureTimeout:
		return who + " timed out"
	default:
		return who + " returned an error"
	}
}

// String renders the headline with the status code appended when there is one:
// "Gemini is overloaded (503)". This is the label form callers paint.
func (f Failure) String() string {
	if f.StatusCode > 0 {
		return fmt.Sprintf("%s (%d)", f.Headline(), f.StatusCode)
	}
	return f.Headline()
}

// providerLabel renders the litellm provider name the way a human writes it.
// An unknown or compat-endpoint name is passed through as-is (it is whatever the
// operator configured); an absent one becomes a neutral noun so a headline never
// reads "  is overloaded".
func (f Failure) providerLabel() string {
	switch strings.ToLower(strings.TrimSpace(f.Provider)) {
	case "":
		return "the model provider"
	case "gemini":
		return "Gemini"
	case "anthropic":
		return "Anthropic"
	case "openai":
		return "OpenAI"
	default:
		return f.Provider
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
