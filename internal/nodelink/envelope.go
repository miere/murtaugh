package nodelink

import (
	"encoding/json"
	"fmt"
)

// Version is the envelope version this build speaks. It is compared for STRICT
// EQUALITY at the top of the read loop rather than negotiated as a range: both
// ends of this link ship from one repository, so a mismatch is a deployment
// error, and a clean refusal at the first frame is a better failure than a
// subtle decode surprise halfway through a turn.
const Version uint16 = 1

// Kind is what a frame is FOR, as far as delivery is concerned. It is the whole
// vocabulary of the envelope; everything a node and the gateway actually say to
// each other is a KindMessage whose Payload this package never looks inside.
type Kind string

const (
	// KindMessage carries a payload. It is the only kind that consumes a
	// sequence number and the only kind that is acknowledged.
	KindMessage Kind = "msg"
	// KindAck carries an acknowledgement and nothing else, for when there is no
	// outbound traffic to piggyback on.
	KindAck Kind = "ack"
	// KindResume asks a peer to replay the suffix this side never consumed,
	// after the connection underneath was replaced.
	KindResume Kind = "resume"
	// KindResumed answers a KindResume: either the replay follows, or it does
	// not and Reason says why.
	KindResumed Kind = "resumed"
)

// Envelope is one frame on the wire.
//
// Five fields, and no payload decoder ever sees any of them. That is the point:
// sequencing and acknowledgement are properties of the envelope, so a payload
// type cannot acquire a "seq" field by accident and a payload change cannot
// break delivery.
type Envelope struct {
	V    uint16 `json:"v"`
	Kind Kind   `json:"k"`
	// Seq is this frame's position in the sender's stream, from 1. Zero on
	// every kind but KindMessage.
	Seq uint64 `json:"seq,omitempty"`
	// Ack is the highest contiguous sequence the SENDER of this frame has
	// handed to its consumer. Zero means "nothing consumed yet", which is why
	// sequences start at 1.
	Ack uint64 `json:"ack,omitempty"`
	// Resume is present only on KindResume and KindResumed.
	Resume *Resume `json:"r,omitempty"`
	// Payload is the wrapped message, opaque here. It must be a single valid
	// JSON document — one frame is one document, which is what keeps a frame
	// legible in a log — but nothing in this package interprets it.
	Payload json.RawMessage `json:"p,omitempty"`
}

// Resume is the reconnect exchange: the request half carries what the asking
// side last consumed, and the answer half says whether the suffix after it can
// still be replayed.
//
// Epoch binds the request to a leadership term. A node that resumes into a
// gateway which was re-promoted since is asking a process that never held the
// buffer, and answering "yes" there would silently drop the very frames the
// resume exists to recover.
type Resume struct {
	LastSeen uint64 `json:"last_seen"`
	Epoch    uint64 `json:"epoch,omitempty"`
	// OK and Reason are the answer half. A refusal is explicit so the caller
	// fails its open turns rather than continuing into a hole.
	OK     bool   `json:"ok,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Message builds a payload frame. seq and ack are supplied by the link.
func Message(seq, ack uint64, payload json.RawMessage) Envelope {
	return Envelope{V: Version, Kind: KindMessage, Seq: seq, Ack: ack, Payload: payload}
}

// Ack builds a standalone acknowledgement. It consumes no sequence number.
func Ack(ack uint64) Envelope {
	return Envelope{V: Version, Kind: KindAck, Ack: ack}
}

// Validate reports whether the frame respects the envelope's invariants. It is
// applied on decode, so a peer that violates them fails the link at the frame
// that broke the rule instead of somewhere downstream.
func (e Envelope) Validate() error {
	if e.V == 0 {
		return fmt.Errorf("nodelink: frame carries no version")
	}
	switch e.Kind {
	case KindMessage:
		if e.Seq == 0 {
			return fmt.Errorf("nodelink: message frame carries no sequence number")
		}
	case KindAck:
		if e.Seq != 0 {
			return fmt.Errorf("nodelink: ack frame consumed sequence %d", e.Seq)
		}
	case KindResume, KindResumed:
		if e.Seq != 0 {
			return fmt.Errorf("nodelink: %s frame consumed sequence %d", e.Kind, e.Seq)
		}
		if e.Resume == nil {
			return fmt.Errorf("nodelink: %s frame carries no resume state", e.Kind)
		}
	default:
		return fmt.Errorf("nodelink: unknown frame kind %q", e.Kind)
	}
	if e.Resume != nil && e.Kind != KindResume && e.Kind != KindResumed {
		return fmt.Errorf("nodelink: %s frame carries resume state", e.Kind)
	}
	return nil
}

// Encode renders the frame for the transport.
func Encode(e Envelope) ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(e)
}

// Decode parses a frame and enforces the invariants.
func Decode(raw []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return Envelope{}, fmt.Errorf("nodelink: decode frame: %w", err)
	}
	if err := e.Validate(); err != nil {
		return Envelope{}, err
	}
	return e, nil
}
