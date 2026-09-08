// Package providerfail is the vocabulary a model-provider failure is reduced
// to: what went wrong, who said so, what a human should do about it.
//
// It is a leaf — it imports nothing of ours and nothing of litellm's — and that
// is the whole reason it exists as its own package rather than staying inside
// internal/llm. Three consumers want this vocabulary and only one of them runs a
// model:
//
//   - internal/agent/native classifies a provider error at the point it happens.
//   - internal/agentwire carries that classification across the wire, so a
//     failure classified on a runtime node reads identically on the gateway.
//   - internal/slack/gateway paints it on the alert card.
//
// The gateway must remain incapable of reaching internal/llm (#170 Change E),
// and internal/llm cannot shed litellm — Classify's other arm matches litellm's
// concrete error type. Splitting the vocabulary from the mapping lets the
// gateway and the wire keep the words without linking a provider client.
package providerfail

import (
	"errors"
	"fmt"
	"strings"
)

// Kind is the coarse reason a provider call failed. It is the vocabulary the UI
// reasons about: every kind answers "is this the user's problem, ours, or the
// provider's, and is retrying worth anything?".
type Kind string

const (
	// Auth — the credential was rejected (401/403). Ours to fix.
	Auth Kind = "auth"
	// RateLimit — we are sending too fast (429). Transient.
	RateLimit Kind = "rate_limit"
	// Overloaded — the provider is out of capacity (503/529). Transient
	// and not our fault; the canonical "try again in a minute" case.
	Overloaded Kind = "overloaded"
	// Quota — billing/quota exhausted (402). Ours to fix, not transient.
	Quota Kind = "quota"
	// ContextOverflow — the conversation exceeds the model's window.
	ContextOverflow Kind = "context_overflow"
	// Model — the model name is unknown to the provider (404).
	Model Kind = "model"
	// Validation — the provider rejected the request shape (400).
	Validation Kind = "validation"
	// Network — we could not reach the provider at all.
	Network Kind = "network"
	// Timeout — the provider did not answer in time.
	Timeout Kind = "timeout"
	// Provider — a server-side error that is none of the above (5xx).
	Provider Kind = "provider"
)

// Failure is a provider error reduced to what a caller can act on: what went
// wrong (Kind), who said so (Provider/StatusCode), the provider's own sentence
// for a human (Message), and whether retrying the identical request could
// plausibly succeed (Retryable).
//
// It exists so callers — the agent loop deciding whether to retry, the Slack
// renderer deciding what to paint — never sniff error strings. The one
// string-shaped job (digging the sentence out of a JSON body) is done exactly
// once, by internal/llm, before a Failure is built.
type Failure struct {
	Kind       Kind
	Provider   string
	StatusCode int
	Message    string
	Retryable  bool
}

// Classify returns the Failure an error carries, if it carries one.
//
// It answers only for errors that were classified at their source — by
// Carrying, on the machine that made the provider call, or by New on the far
// side of the wire. It deliberately does NOT know how to classify a raw provider
// error: that requires litellm's concrete type, which is exactly the dependency
// this package exists to keep out of the gateway. internal/llm.Classify is the
// full-fat version and is the one a producer calls.
func Classify(err error) (Failure, bool) {
	if err == nil {
		return Failure{}, false
	}
	var carried *failureError
	if errors.As(err, &carried) {
		return carried.failure, true
	}
	return Failure{}, false
}

// New returns an error that carries f as its already-derived classification and
// text as its message, so Classify answers with f. It wraps nothing: use it when
// the original error no longer exists.
//
// It exists for the wire. A raw *providers.LiteLLMError cannot survive
// serialisation, so an agent running on a remote node would have every provider
// failure arrive unclassified — silently downgrading "Gemini is overloaded
// (503) — try again in a moment" to the generic "Murtaugh hit an error" card on
// both the foreground and the background rendering paths.
//
// Carrying the Failure rather than rebuilding a *providers.LiteLLMError is
// deliberate: reconstructing a third-party struct would tie the protocol to
// litellm's internals and to whatever they change next, while the vocabulary
// this package owns is exactly what callers act on.
//
// text should be the original error's full text — the alert card puts it in
// Detail verbatim and the session journal records it as the turn's error.
func New(f Failure, text string) error {
	return &failureError{failure: f, text: text}
}

// Carrying attaches f to cause without changing what the error says or what it
// unwraps to: the text is cause's own, and errors.Is/As still see straight
// through to cause. It is what a producer calls at the boundary where an error
// stops being a provider's and becomes the agent layer's, so that every reader
// downstream — in this process or across a link — classifies it identically
// without re-deriving anything.
//
// A nil cause returns nil, so a caller need not branch.
func Carrying(f Failure, cause error) error {
	if cause == nil {
		return nil
	}
	return &failureError{failure: f, text: cause.Error(), cause: cause}
}

// failureError is the carrier behind New and Carrying. It is unexported because
// there is nothing to do with it but hand it to Classify: the Failure is the
// contract, not the wrapper.
type failureError struct {
	failure Failure
	text    string
	// cause is the error this classification was derived from, or nil when the
	// classification arrived without one (the wire's case). Unwrap returns it so
	// attaching a Failure never costs an identity comparison further down.
	cause error
}

func (e *failureError) Error() string { return e.text }

func (e *failureError) Unwrap() error { return e.cause }

// Headline is a one-line human summary naming the provider and what it did —
// "Gemini is overloaded", "OpenAI rejected the credentials". It carries no
// markup: the transport (Slack, a log line) decorates it.
func (f Failure) Headline() string {
	who := f.providerLabel()
	switch f.Kind {
	case Auth:
		return who + " rejected the credentials"
	case RateLimit:
		return who + " is rate limiting us"
	case Overloaded:
		return who + " is overloaded"
	case Quota:
		return "the " + who + " account is out of quota"
	case ContextOverflow:
		return "the conversation is too long for this model"
	case Model:
		return who + " does not know this model"
	case Validation:
		return who + " rejected the request"
	case Network:
		return who + " is unreachable"
	case Timeout:
		return who + " timed out"
	default:
		return who + " returned an error"
	}
}

// Remedy is what the person reading the failure should do about it, in one
// sentence. It sits beside Headline because it follows from the Kind and nothing
// else: whose quota ran out is a fact about the provider, not about the surface
// the message is painted on.
//
// It is deliberately addressed to the reader rather than to the operator: on a
// personal deployment they are the same person, and on a shared one the reader
// still needs to know whether to wait, rephrase, or fetch somebody.
func (f Failure) Remedy() string {
	switch f.Kind {
	case Auth:
		return "The configured credentials need attention — notify your admin user."
	case RateLimit, Overloaded:
		return "Try again in a moment."
	case Quota:
		return "The account needs more quota — notify your admin user."
	case ContextOverflow:
		return "Start a fresh thread: this conversation no longer fits the model's context."
	case Model:
		return "The configured model name looks wrong — notify your admin user."
	case Validation:
		return "Try rephrasing. If it keeps happening, notify your admin user."
	case Network, Timeout:
		return "Try again. If it keeps happening, check the network path to the provider."
	default:
		return "Try again. If it keeps happening, notify your admin user."
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
