package agentwire

import (
	"encoding/json"
	"fmt"
)

// MessageKind is what one payload IS: a call, its answer, an event on a turn's
// stream, an answer to a permission request, or a chunk of an attachment's
// bytes.
//
// It is the payload's own framing and it lives here rather than in the
// envelope, because the envelope must be able to deliver a frame it cannot
// read. Sequencing and acknowledgement are the envelope's; what a frame means
// is this package's.
type MessageKind string

const (
	// MessageRequest is a call. ID is minted by the caller and echoed in the
	// answer; Method names what is being asked.
	MessageRequest MessageKind = "req"
	// MessageResponse answers exactly one MessageRequest. A non-nil Error is a
	// rejection: the call did not happen.
	MessageResponse MessageKind = "res"
	// MessageEvent is one item on an ordered event stream. ID names the request
	// whose stream it belongs to; End marks the last one, which is what closes
	// the consumer's Go channel.
	MessageEvent MessageKind = "evt"
	// MessagePermission answers a PermissionRequest raised by an event. It is
	// not a MessageResponse because it answers an EVENT, not a call, and it
	// travels in the opposite direction to the rest of this list.
	MessagePermission MessageKind = "perm"
	// MessageChunk is one frame of an attachment's side transfer, correlated by
	// TransferID rather than by request id.
	MessageChunk MessageKind = "chunk"
)

// Method is the request vocabulary, and it is not one list but two.
//
// Gateway → node is agent.Client's five methods plus session.close, which is not
// on the interface but is asserted for on the concrete client and tears down a
// real per-conversation process on two of the three backends.
//
// Node → gateway is the tool channel: tool.list and tool.call. It is the only
// direction a node initiates a call in, and the direction ids for it are minted
// by the NODE — see the note on Message about ids being per direction. A shared
// pending map would collapse two live requests both numbered "1".
type Method string

const (
	MethodInitialize   Method = "initialize"
	MethodNewSession   Method = "session.new"
	MethodPrompt       Method = "prompt"
	MethodCancel       Method = "cancel"
	MethodCloseSession Method = "session.close"
	MethodClose        Method = "close"

	// MethodToolList asks the gateway which of Murtaugh's tools this node may
	// reach. The answer is the partition applied (toolset.Reach), not the
	// gateway's registry: a node never learns the name of a tool it may not
	// call.
	MethodToolList Method = "tool.list"
	// MethodToolCall runs one of them, on the gateway, with the gateway's
	// credentials. The gateway re-checks the partition on every call — a node
	// that names a tool it was not offered is refused rather than filtered,
	// because a node cannot be trusted to filter itself.
	MethodToolCall Method = "tool.call"
	// MethodAdvertise replaces what the gateway believes this node claims. Its
	// body is an Advertisement and its answer is Empty.
	//
	// It is a request rather than a fire-and-forget event so the node learns
	// whether the claim landed: a node whose advertisement was rejected and
	// which cannot tell would go on believing it serves channels the gateway
	// will never send it. It carries only CHANGES — the connect-time snapshot
	// rides InitializeResult, because a node-initiated frame at handshake time
	// can beat the gateway's own registry entry into existence.
	MethodAdvertise Method = "node.advertise"
	// MethodConfigure hands a node that has never been configured the agent
	// profiles a Slack onboarding form just produced for its owner. Its body is
	// a NodeConfiguration and its answer is a NodeConfigured.
	//
	// It is the only method that carries configuration, and the only one a node
	// may refuse on policy: a node applies it solely while it holds no agent
	// profile of its own, so it can bootstrap an empty node and can never
	// reconfigure a running one. See configure.go for why that guarantee lives
	// on the node rather than in this constant.
	MethodConfigure Method = "node.configure"
)

// Message is one payload: what the envelope wraps.
//
// Request ids are per DIRECTION and are only ever matched within the direction
// that minted them, the same rule the ACP transport's per-connection counter
// follows. Nothing here is a sequence number: two messages with no ordering
// relationship must not appear to have one.
type Message struct {
	Kind MessageKind `json:"k"`
	// ID correlates a request with its response, and an event with the request
	// whose stream it belongs to. Empty on an unsolicited event and on a chunk.
	ID string `json:"id,omitempty"`
	// Method is set on a request only.
	Method Method `json:"method,omitempty"`
	// SessionID addresses an event that belongs to no request: the background
	// path, where a subagent completes after its turn ended and the events are
	// keyed by session id alone. Without it that whole stream would have
	// nowhere to go, because it was never anybody's answer.
	SessionID string `json:"session_id,omitempty"`
	// End marks the final event of a stream. It is a field rather than an event
	// kind because a stream can end WITHOUT a terminal event — the in-process
	// contract is that the channel closes, and two consumers block on exactly
	// that.
	End bool `json:"end,omitempty"`
	// Error is a rejected request. It carries the same discriminant vocabulary
	// as an event's error, so a call that fails for a reason the gateway
	// branches on keeps that branch.
	Error *Error `json:"error,omitempty"`
	// Body is the method's request or result payload, or the event/chunk/
	// permission answer itself.
	Body json.RawMessage `json:"body,omitempty"`
}

// Encode renders a message for the envelope to carry.
func (m Message) Encode() ([]byte, error) { return json.Marshal(m) }

// DecodeMessage parses one payload delivered by the envelope.
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

// Fault returns the error a rejected response carries, or nil.
//
// It rebuilds the same discriminant-bearing error an event's would, so a call
// that failed because the turn was cancelled still satisfies errors.Is against
// context.Canceled on the far side. It is a method rather than an exported
// decodeError so the rebuilding stays in one place.
func (m Message) Fault() error { return decodeError(m.Error) }

// Into unmarshals the message body into v.
//
// An absent body is not an error: several methods take or return nothing, and
// v is left at its zero value.
func (m Message) Into(v any) error {
	if len(m.Body) == 0 {
		return nil
	}
	if err := json.Unmarshal(m.Body, v); err != nil {
		return fmt.Errorf("agentwire: decode %s body: %w", m.Kind, err)
	}
	return nil
}

// Request builds a call. body may be nil for a method that takes nothing.
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

// Result builds a successful answer to the request identified by id.
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

// Fault builds a rejected answer.
//
// The error is classified once, here, by the same encodeError that classifies
// an event's — so a session that could not be created because the turn was
// cancelled is still a cancellation to the gateway, not a generic failure card.
func Fault(id string, err error) Message {
	return Message{Kind: MessageResponse, ID: id, Error: encodeError(err)}
}

// StreamEvent builds one event on the stream belonging to request streamID.
func StreamEvent(streamID string, ev Event) (Message, error) {
	encoded, err := json.Marshal(ev)
	if err != nil {
		return Message{}, fmt.Errorf("agentwire: encode event: %w", err)
	}
	return Message{Kind: MessageEvent, ID: streamID, Body: encoded}, nil
}

// StreamEnd closes the stream belonging to request streamID. It carries no
// event: the in-process equivalent is closing the channel, which also carries
// nothing, and both consumers treat a closed channel as a legitimate end.
func StreamEnd(streamID string) Message {
	return Message{Kind: MessageEvent, ID: streamID, End: true}
}

// BackgroundEvent builds an event that belongs to no request: a background turn
// completing against a session the gateway is no longer prompting.
func BackgroundEvent(sessionID string, ev Event) (Message, error) {
	encoded, err := json.Marshal(ev)
	if err != nil {
		return Message{}, fmt.Errorf("agentwire: encode background event: %w", err)
	}
	return Message{Kind: MessageEvent, SessionID: sessionID, Body: encoded}, nil
}

// PermissionAnswer builds the frame that answers a permission request raised on
// an event stream.
func PermissionAnswer(resp PermissionResponse) (Message, error) {
	encoded, err := json.Marshal(resp)
	if err != nil {
		return Message{}, fmt.Errorf("agentwire: encode permission answer: %w", err)
	}
	return Message{Kind: MessagePermission, ID: resp.ID, Body: encoded}, nil
}

// Chunk builds one frame of an attachment's side transfer.
func Chunk(c TransferChunk) (Message, error) {
	encoded, err := json.Marshal(c)
	if err != nil {
		return Message{}, fmt.Errorf("agentwire: encode transfer chunk: %w", err)
	}
	return Message{Kind: MessageChunk, ID: c.TransferID, Body: encoded}, nil
}
