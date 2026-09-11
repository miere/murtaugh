package nodelink

import (
	"encoding/json"
	"go/build"
	"strings"
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		env  Envelope
	}{
		{"a message", Message(1, 0, json.RawMessage(`{"k":"req"}`))},
		{"a message acknowledging as it goes", Message(9, 8, json.RawMessage(`"text"`))},
		{"an empty payload", Message(2, 1, nil)},
		{"a standalone ack", Ack(12)},
		{"a resume request", Envelope{V: Version, Kind: KindResume, Resume: &Resume{LastSeen: 4, Epoch: 7}}},
		{"a refused resume", Envelope{V: Version, Kind: KindResumed, Resume: &Resume{LastSeen: 4, Epoch: 7, Reason: "released"}}},
		{"a payload wearing the envelope's field names", Message(3, 2, json.RawMessage(`{"v":99,"k":"ack","seq":1000,"ack":1000}`))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := Encode(tc.env)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			back, err := Decode(raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if back.V != tc.env.V || back.Kind != tc.env.Kind || back.Seq != tc.env.Seq || back.Ack != tc.env.Ack {
				t.Fatalf("header came back as %+v, want %+v", back, tc.env)
			}
			if string(back.Payload) != string(tc.env.Payload) {
				t.Fatalf("payload came back as %q, want %q", back.Payload, tc.env.Payload)
			}
			if (back.Resume == nil) != (tc.env.Resume == nil) {
				t.Fatalf("resume presence changed: %+v", back.Resume)
			}
			if back.Resume != nil && *back.Resume != *tc.env.Resume {
				t.Fatalf("resume came back as %+v, want %+v", *back.Resume, *tc.env.Resume)
			}
		})
	}
}

func TestEnvelopeInvariants(t *testing.T) {
	cases := []struct {
		name string
		env  Envelope
	}{
		{"a message with no sequence", Envelope{V: Version, Kind: KindMessage}},
		{"an ack that consumed a sequence", Envelope{V: Version, Kind: KindAck, Seq: 3, Ack: 2}},
		{"a resume that consumed a sequence", Envelope{V: Version, Kind: KindResume, Seq: 3, Resume: &Resume{}}},
		{"a resume with no state", Envelope{V: Version, Kind: KindResume}},
		{"a message carrying resume state", Envelope{V: Version, Kind: KindMessage, Seq: 1, Resume: &Resume{}}},
		{"an unknown kind", Envelope{V: Version, Kind: "shout", Seq: 1}},
		{"no version", Envelope{Kind: KindMessage, Seq: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.env.Validate(); err == nil {
				t.Fatal("the frame validated; it must not")
			}
			if _, err := Encode(tc.env); err == nil {
				t.Fatal("the frame encoded; it must not")
			}
		})
	}
}

// Checking direct imports is enough: Murtaugh packages are internal to this module, so no
// third-party dependency can import one on our behalf.
func TestEnvelopeKnowsNothingOfMurtaugh(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("read package: %v", err)
	}
	for _, imported := range pkg.Imports {
		if strings.HasPrefix(imported, "github.com/miere/murtaugh/") {
			t.Fatalf("the envelope imports %s; delivery must not be able to see what it delivers", imported)
		}
	}
}
