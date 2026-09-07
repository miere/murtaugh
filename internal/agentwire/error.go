package agentwire

import (
	"context"
	"errors"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/llm"
)

// ErrorKind is the wire's discriminant vocabulary: the condition an error
// names, decided once by the producer and mapped back to a sentinel by the
// consumer.
//
// It exists because the gateway compares Event.Error by IDENTITY, and
// serialisation preserves an error's text and destroys its identity without
// anything failing loudly. A user interrupt would start rendering as a failure
// card; a wedged session would stop being dropped. Every consumer on this path
// was enumerated before this list was written:
//
//   - errors.Is(err, context.Canceled) — three separate decisions in
//     gateway/chat_handler.go (return-as-interrupt, the deferred "_interrupted_"
//     marker, the deferred session-log outcome) → ErrorCancelled. A fourth site
//     in refreshAssistantStatus tests a Slack API error, which never comes off
//     this wire, so it is not a consumer of this vocabulary.
//   - errors.Is(err, agent.ErrToolCeiling) — drops the session binding when a
//     backend cannot stop a wedged tool → ErrorToolCeiling.
//   - llm.Classify(err) — gateway/alert.go's failSpec, reached from every
//     renderer.Fail on both the foreground and background paths. It does not
//     merely branch: it reads five structured fields off a third-party concrete
//     error to build the user-facing card → ErrorProvider.
//   - acp.IsMethodNotFound-shaped reads of a JSON-RPC code → ErrorRPC.
//   - claudecode.IsAuthFailure(err) — gateway/credrepair.go. This one matches on
//     PROSE, not identity, which is why Message is the full original text
//     verbatim rather than a summary, and why it needs no discriminant of its
//     own. (It must not have one: credrepair deliberately also checks the
//     agent's configured kind, and that config lives on the gateway. A node
//     asserting "this was a credential failure" would move that policy to the
//     wrong end.)
//
// The vocabulary is closed and small on purpose. An unrecognised Kind degrades
// to exactly today's generic behaviour — the full text, no sentinel — rather
// than losing information, and nodes cannot invent discriminants the gateway
// does not understand.
type ErrorKind string

const (
	// ErrorUnknown — no discriminant applies. The text is all there is, which
	// is what an in-process consumer would have had anyway.
	ErrorUnknown ErrorKind = "unknown"
	// ErrorCancelled — the turn was interrupted. Decodes to an error that
	// satisfies errors.Is(err, context.Canceled).
	ErrorCancelled ErrorKind = "cancelled"
	// ErrorToolCeiling — a tool ran past its execution ceiling. Decodes to an
	// error that satisfies errors.Is(err, agent.ErrToolCeiling).
	ErrorToolCeiling ErrorKind = "tool_ceiling"
	// ErrorProvider — the model provider failed, already classified into the
	// llm.Failure vocabulary. Carries Provider.
	ErrorProvider ErrorKind = "provider"
	// ErrorRPC — a JSON-RPC fault from an external agent process. Carries RPC.
	ErrorRPC ErrorKind = "rpc"
)

// Error is the serialisable form of agent.Event's error.
//
// Message is the producing error's full text, unabridged and verbatim. That is
// not belt-and-braces: it is what the alert card puts in Detail, what the
// session journal records as errText, and what credrepair substring-matches to
// decide whether the admin is asked to re-authenticate. A discriminant without
// the text would keep the branch and lose the diagnosis.
type Error struct {
	Kind     ErrorKind        `json:"kind"`
	Message  string           `json:"message"`
	Provider *ProviderFailure `json:"provider,omitempty"`
	RPC      *RPCFailure      `json:"rpc,omitempty"`
}

// ProviderFailure is llm.Failure on the wire: the classification the producer
// already derived, carried so the consumer does not have to re-derive it from a
// third-party error type that cannot survive serialisation.
type ProviderFailure struct {
	Kind       string `json:"kind"`
	Provider   string `json:"provider,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
	Message    string `json:"message,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
}

// RPCFailure is a JSON-RPC fault's structured part: which method faulted and
// the numeric code, which is the half a text-only wire flattens.
type RPCFailure struct {
	Method string `json:"method,omitempty"`
	Code   int    `json:"code"`
}

// RPCFaulter is an error that can describe its JSON-RPC fault in structured
// form. The ACP backend's *RPCError satisfies it; the decoded form of an
// ErrorRPC satisfies it too, so a consumer reads the code the same way on
// either side of the wire.
//
// It is declared here, and matched structurally, so this package never imports
// an agent backend — the gateway has to be able to import the protocol while
// remaining incapable of running an agent.
type RPCFaulter interface {
	error
	// RPCFault returns the faulting method and the JSON-RPC error code.
	RPCFault() (method string, code int)
}

// encodeError classifies err once, at the producer, and records the result.
//
// The order matters and mirrors the gateway's own branching: chat_handler tests
// cancellation before the tool ceiling, so an error that somehow satisfied both
// is a cancellation to the consumer and must be one on the wire too.
func encodeError(err error) *Error {
	if err == nil {
		return nil
	}
	w := &Error{Kind: ErrorUnknown, Message: err.Error()}

	switch {
	case errors.Is(err, context.Canceled):
		w.Kind = ErrorCancelled
		return w
	case errors.Is(err, agent.ErrToolCeiling):
		w.Kind = ErrorToolCeiling
		return w
	}

	if failure, ok := llm.Classify(err); ok {
		w.Kind = ErrorProvider
		w.Provider = &ProviderFailure{
			Kind:       string(failure.Kind),
			Provider:   failure.Provider,
			StatusCode: failure.StatusCode,
			Message:    failure.Message,
			Retryable:  failure.Retryable,
		}
		return w
	}

	var fault RPCFaulter
	if errors.As(err, &fault) {
		method, code := fault.RPCFault()
		w.Kind = ErrorRPC
		w.RPC = &RPCFailure{Method: method, Code: code}
		return w
	}

	return w
}

// decodeError rebuilds an error that answers the consumer's identity
// comparisons the way the original would have, while reading — to a log, to a
// card's detail block, to the journal — exactly like the original.
func decodeError(w *Error) error {
	if w == nil {
		return nil
	}
	switch w.Kind {
	case ErrorCancelled:
		return &wireError{msg: w.Message, sentinel: context.Canceled}
	case ErrorToolCeiling:
		return &wireError{msg: w.Message, sentinel: agent.ErrToolCeiling}
	case ErrorProvider:
		if w.Provider != nil {
			return llm.NewFailureError(llm.Failure{
				Kind:       llm.FailureKind(w.Provider.Kind),
				Provider:   w.Provider.Provider,
				StatusCode: w.Provider.StatusCode,
				Message:    w.Provider.Message,
				Retryable:  w.Provider.Retryable,
			}, w.Message)
		}
	case ErrorRPC:
		if w.RPC != nil {
			return &rpcError{
				wireError: wireError{msg: w.Message},
				method:    w.RPC.Method,
				code:      w.RPC.Code,
			}
		}
	}
	return &wireError{msg: w.Message}
}

// wireError is a decoded Event.Error: the producer's exact message with the
// sentinel its Kind names re-attached underneath.
//
// Both halves are load-bearing. Returning the bare sentinel would satisfy
// errors.Is and throw away the prose — and the prose is the whole diagnostic
// value of a tool ceiling ("terminal ran for 5m0s with no result") and what
// lands in the alert card and the journal. Returning errors.New(msg) would keep
// the prose and silently turn a user's interrupt into a failure card.
type wireError struct {
	msg string
	// sentinel is what errors.Is must find, or nil when the Kind names none.
	sentinel error
}

func (e *wireError) Error() string { return e.msg }

// Unwrap exposes the sentinel to errors.Is. A nil sentinel simply ends the
// chain, which is the right answer for an error that carried no discriminant.
func (e *wireError) Unwrap() error { return e.sentinel }

// rpcError is a decoded ErrorRPC: a wireError that can still be asked for its
// JSON-RPC code.
type rpcError struct {
	wireError
	method string
	code   int
}

// RPCFault satisfies RPCFaulter, so a consumer reading the code does the same
// errors.As on a decoded fault as on a locally produced one.
func (e *rpcError) RPCFault() (string, int) { return e.method, e.code }
