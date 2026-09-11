package nodelink

import (
	"encoding/json"
	"fmt"
)

// Compared for strict equality, not negotiated: both ends ship from one repo, so a mismatch is a
// deployment error, best refused at the first frame.
const Version uint16 = 1

type Kind string

const (
	KindMessage Kind = "msg"
	// An ack takes no sequence number, or two idle peers would acknowledge each other's acks forever.
	KindAck     Kind = "ack"
	KindResume  Kind = "resume"
	KindResumed Kind = "resumed"
)

type Envelope struct {
	V    uint16 `json:"v"`
	Kind Kind   `json:"k"`
	Seq  uint64 `json:"seq,omitempty"`
	// Zero means nothing consumed yet, which is why sequences start at 1.
	Ack     uint64          `json:"ack,omitempty"`
	Resume  *Resume         `json:"r,omitempty"`
	Payload json.RawMessage `json:"p,omitempty"`
}

type Resume struct {
	LastSeen uint64 `json:"last_seen"`
	Epoch    uint64 `json:"epoch,omitempty"`
	OK       bool   `json:"ok,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func Message(seq, ack uint64, payload json.RawMessage) Envelope {
	return Envelope{V: Version, Kind: KindMessage, Seq: seq, Ack: ack, Payload: payload}
}

func Ack(ack uint64) Envelope {
	return Envelope{V: Version, Kind: KindAck, Ack: ack}
}

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

func Encode(e Envelope) ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(e)
}

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
