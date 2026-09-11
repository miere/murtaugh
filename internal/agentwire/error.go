package agentwire

import (
	"context"
	"errors"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/providerfail"
)

// ErrorKind exists because the gateway compares errors by identity, which serialisation
// destroys. An unknown Kind degrades to the plain text rather than losing information.
type ErrorKind string

const (
	ErrorUnknown     ErrorKind = "unknown"
	ErrorCancelled   ErrorKind = "cancelled"
	ErrorToolCeiling ErrorKind = "tool_ceiling"
	ErrorProvider    ErrorKind = "provider"
	ErrorRPC         ErrorKind = "rpc"
	// ErrorCredential lets the gateway tell the user the owner already has a
	// sign-in in front of them, instead of starting one itself.
	ErrorCredential ErrorKind = "credential"
)

// Error keeps the producer's full text beside its Kind, because a discriminant
// without the text would keep the branch and lose the diagnosis.
type Error struct {
	Kind     ErrorKind        `json:"kind"`
	Message  string           `json:"message"`
	Provider *ProviderFailure `json:"provider,omitempty"`
	RPC      *RPCFailure      `json:"rpc,omitempty"`
}

// ProviderFailure carries the producer's classification because the third-party error it came
// from cannot survive serialisation.
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

// RPCFaulter is matched structurally so this package never imports an agent backend: the
// gateway must import the protocol yet stay unable to run an agent.
type RPCFaulter interface {
	error
	RPCFault() (method string, code int)
}

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
	case errors.Is(err, agent.ErrCredentialRejected):
		w.Kind = ErrorCredential
		return w
	}

	if failure, ok := providerfail.Classify(err); ok {
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

func decodeError(w *Error) error {
	if w == nil {
		return nil
	}
	switch w.Kind {
	case ErrorCancelled:
		return &wireError{msg: w.Message, sentinel: context.Canceled}
	case ErrorToolCeiling:
		return &wireError{msg: w.Message, sentinel: agent.ErrToolCeiling}
	case ErrorCredential:
		return &wireError{msg: w.Message, sentinel: agent.ErrCredentialRejected}
	case ErrorProvider:
		if w.Provider != nil {
			return providerfail.New(providerfail.Failure{
				Kind:       providerfail.Kind(w.Provider.Kind),
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

type wireError struct {
	msg      string
	sentinel error
}

func (e *wireError) Error() string { return e.msg }

func (e *wireError) Unwrap() error { return e.sentinel }

type rpcError struct {
	wireError
	method string
	code   int
}

func (e *rpcError) RPCFault() (string, int) { return e.method, e.code }
