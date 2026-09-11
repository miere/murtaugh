// Package providerfail must stay a leaf that imports nothing of litellm's, so the
// gateway can show provider failures without linking a provider client.
package providerfail

import (
	"errors"
	"fmt"
	"strings"
)

type Kind string

const (
	Auth            Kind = "auth"
	RateLimit       Kind = "rate_limit"
	Overloaded      Kind = "overloaded"
	Quota           Kind = "quota"
	ContextOverflow Kind = "context_overflow"
	Model           Kind = "model"
	Validation      Kind = "validation"
	Network         Kind = "network"
	Timeout         Kind = "timeout"
	Provider        Kind = "provider"
)

// Failure exists so callers never sniff error strings; the one string-parsing
// job is done once, in internal/llm, before a Failure is built.
type Failure struct {
	Kind       Kind
	Provider   string
	StatusCode int
	Message    string
	Retryable  bool
}

// Classify only knows errors classified at their source: raw provider errors need
// litellm, which this package must not import. internal/llm.Classify handles those.
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

// New exists for the wire: a raw litellm error cannot be serialised, so without it
// a remote node's provider failures would reach the gateway unclassified.
func New(f Failure, text string) error {
	return &failureError{failure: f, text: text}
}

func Carrying(f Failure, cause error) error {
	if cause == nil {
		return nil
	}
	return &failureError{failure: f, text: cause.Error(), cause: cause}
}

type failureError struct {
	failure Failure
	text    string
	cause   error
}

func (e *failureError) Error() string { return e.text }

func (e *failureError) Unwrap() error { return e.cause }

// Headline carries no markup because each transport (Slack, a log line) adds its own.
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

// Remedy speaks to whoever reads the failure, not the operator: on a shared
// deployment they still need to know whether to wait, rephrase or fetch someone.
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

func (f Failure) String() string {
	if f.StatusCode > 0 {
		return fmt.Sprintf("%s (%d)", f.Headline(), f.StatusCode)
	}
	return f.Headline()
}

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
