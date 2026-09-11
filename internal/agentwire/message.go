package agentwire

import (
	"encoding/json"
	"fmt"
)

// MessageKind lives in the payload, not the envelope, because the envelope must deliver frames
// it cannot read.
type MessageKind string

const (
	MessageRequest  MessageKind = "req"
	MessageResponse MessageKind = "res"
	MessageEvent    MessageKind = "evt"
	// MessagePermission is not a MessageResponse because it answers an event, not a call, and
	// travels the opposite way.
	MessagePermission MessageKind = "perm"
	MessageChunk      MessageKind = "chunk"
)

// Method ids are per direction: node.advertise, the one call a node makes, is
// numbered by the node and must never be matched against the gateway's calls.
type Method string

const (
	MethodInitialize   Method = "initialize"
	MethodNewSession   Method = "session.new"
	MethodPrompt       Method = "prompt"
	MethodCancel       Method = "cancel"
	MethodCloseSession Method = "session.close"
	MethodClose        Method = "close"

	// MethodAdvertise carries only changes: the connect-time claim rides InitializeResult, because
	// a node-initiated frame during the handshake can beat the gateway's registry entry.
	MethodAdvertise Method = "node.advertise"
	// MethodConfigure is the only method a node may refuse on policy: it applies only while the
	// node has no agent profile of its own, so it can never reconfigure a running node.
	MethodConfigure Method = "node.configure"
	MethodSignIn    Method = "node.sign_in"
	// MethodSignInSettled is answered so the node sends the next update only
	// once the gateway holds the last, which keeps the card's states in order.
	MethodSignInSettled Method = "node.sign_in.settled"
	// MethodCredentialHealth is answered so the node sends its reports one at a
	// time, and a recovery can never arrive ahead of the failure it ends.
	MethodCredentialHealth Method = "node.credential_health"
	MethodRenewCredential  Method = "node.renew_credential"
)

// Request ids are only matched within the direction that minted them, and are not sequence
// numbers: unrelated messages must not look ordered.
type Message struct {
	Kind   MessageKind `json:"k"`
	ID     string      `json:"id,omitempty"`
	Method Method      `json:"method,omitempty"`
	// SessionID addresses background events, which belong to no request and would otherwise
	// have nowhere to go.
	SessionID string `json:"session_id,omitempty"`
	// End is a field, not an event kind, because a stream can end without a terminal event.
	End   bool            `json:"end,omitempty"`
	Error *Error          `json:"error,omitempty"`
	Body  json.RawMessage `json:"body,omitempty"`
}

func (m Message) Encode() ([]byte, error) { return json.Marshal(m) }

func DecodeMessage(raw []byte) (Message, error) {
	var m Message
	if err := json.Unmarshal(raw, &m); err != nil {
		return Message{}, fmt.Errorf("agentwire: decode message: %w", err)
	}
	if m.Kind == "" {
		return Message{}, fmt.Errorf("agentwire: message carries no kind")
	}
	return m, nil
}

// Fault rebuilds the discriminant-bearing error, so a call cancelled on the node still
// matches context.Canceled here.
func (m Message) Fault() error { return decodeError(m.Error) }

// An absent body is not an error: several methods take or return nothing.
func (m Message) Into(v any) error {
	if len(m.Body) == 0 {
		return nil
	}
	if err := json.Unmarshal(m.Body, v); err != nil {
		return fmt.Errorf("agentwire: decode %s body: %w", m.Kind, err)
	}
	return nil
}

func Request(id string, method Method, body any) (Message, error) {
	m := Message{Kind: MessageRequest, ID: id, Method: method}
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return Message{}, fmt.Errorf("agentwire: encode %s request: %w", method, err)
		}
		m.Body = encoded
	}
	return m, nil
}

func Result(id string, body any) (Message, error) {
	m := Message{Kind: MessageResponse, ID: id}
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return Message{}, fmt.Errorf("agentwire: encode result: %w", err)
		}
		m.Body = encoded
	}
	return m, nil
}

func Fault(id string, err error) Message {
	return Message{Kind: MessageResponse, ID: id, Error: encodeError(err)}
}

func StreamEvent(streamID string, ev Event) (Message, error) {
	encoded, err := json.Marshal(ev)
	if err != nil {
		return Message{}, fmt.Errorf("agentwire: encode event: %w", err)
	}
	return Message{Kind: MessageEvent, ID: streamID, Body: encoded}, nil
}

func StreamEnd(streamID string) Message {
	return Message{Kind: MessageEvent, ID: streamID, End: true}
}

func BackgroundEvent(sessionID string, ev Event) (Message, error) {
	encoded, err := json.Marshal(ev)
	if err != nil {
		return Message{}, fmt.Errorf("agentwire: encode background event: %w", err)
	}
	return Message{Kind: MessageEvent, SessionID: sessionID, Body: encoded}, nil
}

func PermissionAnswer(resp PermissionResponse) (Message, error) {
	encoded, err := json.Marshal(resp)
	if err != nil {
		return Message{}, fmt.Errorf("agentwire: encode permission answer: %w", err)
	}
	return Message{Kind: MessagePermission, ID: resp.ID, Body: encoded}, nil
}

func Chunk(c TransferChunk) (Message, error) {
	encoded, err := json.Marshal(c)
	if err != nil {
		return Message{}, fmt.Errorf("agentwire: encode transfer chunk: %w", err)
	}
	return Message{Kind: MessageChunk, ID: c.TransferID, Body: encoded}, nil
}
