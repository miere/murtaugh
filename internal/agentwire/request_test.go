package agentwire

import (
	"context"
	"errors"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
)

func tripMessage(t *testing.T, m Message) Message {
	t.Helper()
	raw, err := m.Encode()
	if err != nil {
		t.Fatalf("encode message: %v", err)
	}
	back, err := DecodeMessage(raw)
	if err != nil {
		t.Fatalf("decode message: %v", err)
	}
	return back
}

func TestInitializeRoundTrip(t *testing.T) {
	request, err := Request("1", MethodInitialize, Empty{})
	if err != nil {
		t.Fatalf("build initialize: %v", err)
	}
	back := tripMessage(t, request)
	if back.Kind != MessageRequest || back.Method != MethodInitialize || back.ID != "1" {
		t.Fatalf("initialize request came back as %+v", back)
	}

	yes, no := true, false
	cases := []struct {
		name  string
		value *bool
		want  bool
	}{
		{"interruptible", &yes, true},
		{"not interruptible", &no, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Result("1", InitializeResult{Interruptible: tc.value})
			if err != nil {
				t.Fatalf("build result: %v", err)
			}
			var got InitializeResult
			if err := tripMessage(t, result).Into(&got); err != nil {
				t.Fatalf("read result: %v", err)
			}
			if got.Interruptible == nil {
				t.Fatal("interruptible did not survive the round trip")
			}
			if *got.Interruptible != tc.want {
				t.Fatalf("interruptible = %v, want %v", *got.Interruptible, tc.want)
			}
		})
	}
}

func TestInitializeSilenceIsNotADenial(t *testing.T) {
	result, err := Result("1", InitializeResult{})
	if err != nil {
		t.Fatalf("build result: %v", err)
	}
	var got InitializeResult
	if err := tripMessage(t, result).Into(&got); err != nil {
		t.Fatalf("read result: %v", err)
	}
	if got.Interruptible != nil {
		t.Fatalf("an unset interruptible decoded as %v; it must stay unknown", *got.Interruptible)
	}
}

func TestNewSessionRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		meta agent.SessionMetadata
	}{
		{"zero", agent.SessionMetadata{}},
		{"a chat thread", agent.SessionMetadata{
			TeamID: "T1", ChannelID: "C1", ChannelName: "nc-releases",
			ThreadTS: "1700000000.000100", UserID: "U1", Source: "slack",
		}},
		{"a canvas surface", agent.SessionMetadata{
			TeamID: "T1", ChannelID: "C1", UserID: "U1", Source: "slack", Surface: "canvas", CanvasID: "F0123",
		}},
		{"an ephemeral delegation", agent.SessionMetadata{Source: "delegate", Ephemeral: true}},
		{"a headless delegation", agent.SessionMetadata{Source: "delegate", Ephemeral: true, Headless: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request, err := Request("7", MethodNewSession, EncodeSessionMetadata(tc.meta))
			if err != nil {
				t.Fatalf("build session.new: %v", err)
			}
			back := tripMessage(t, request)
			if back.Method != MethodNewSession {
				t.Fatalf("method = %q", back.Method)
			}
			var wire SessionMetadata
			if err := back.Into(&wire); err != nil {
				t.Fatalf("read metadata: %v", err)
			}
			if got := wire.Decode(); got != tc.meta {
				t.Fatalf("metadata came back as %+v, want %+v", got, tc.meta)
			}
			got, want := agent.DeriveSessionID(wire.Decode()), agent.DeriveSessionID(tc.meta)
			if tc.meta.Ephemeral && got == want {
				t.Fatal("an ephemeral session derived a stable id; it must be fresh each time")
			}
			if !tc.meta.Ephemeral && got != want {
				t.Fatalf("derived session id changed across the wire: %q vs %q", got, want)
			}

			result, err := Result("7", NewSessionResult{SessionID: "session-42"})
			if err != nil {
				t.Fatalf("build result: %v", err)
			}
			var out NewSessionResult
			if err := tripMessage(t, result).Into(&out); err != nil {
				t.Fatalf("read result: %v", err)
			}
			if out.SessionID != "session-42" {
				t.Fatalf("session id = %q", out.SessionID)
			}
		})
	}
}

func TestPromptRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		request agent.PromptRequest
	}{
		{"bare text", agent.PromptRequest{Text: "hello"}},
		{"a chat turn", agent.PromptRequest{
			Text: "what changed?", Channel: "C1", Thread: "1700000000.000100", User: "U1",
		}},
		{"a cold session backfilling a thread", agent.PromptRequest{
			Text: "carry on", Channel: "C1", Thread: "1700000000.000100", User: "U1",
			History: "<thread>\n@alice: first\n@bob: second\n</thread>",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request, err := Request("9", MethodPrompt, PromptBody{
				SessionID: "session-42",
				Prompt:    EncodePromptRequest(tc.request),
			})
			if err != nil {
				t.Fatalf("build prompt: %v", err)
			}
			var body PromptBody
			if err := tripMessage(t, request).Into(&body); err != nil {
				t.Fatalf("read prompt: %v", err)
			}
			if body.SessionID != "session-42" {
				t.Fatalf("session id = %q", body.SessionID)
			}
			if got := body.Prompt.Decode(); got != tc.request {
				t.Fatalf("prompt came back as %+v, want %+v", got, tc.request)
			}
		})
	}
}

func TestPromptRejectionCarriesItsDiscriminant(t *testing.T) {
	fault := tripMessage(t, Fault("9", context.Canceled))
	err := fault.Fault()
	if err == nil {
		t.Fatal("a rejected prompt came back with no error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("rejection lost its discriminant: %v", err)
	}
	if err.Error() != context.Canceled.Error() {
		t.Fatalf("rejection text = %q", err.Error())
	}
	accepted := tripMessage(t, mustResult(t, "9", Empty{}))
	if accepted.Fault() != nil {
		t.Fatalf("an accepted prompt carried an error: %v", accepted.Fault())
	}
}

func TestCancelAndSessionCloseRoundTrip(t *testing.T) {
	for _, method := range []Method{MethodCancel, MethodCloseSession} {
		t.Run(string(method), func(t *testing.T) {
			request, err := Request("3", method, SessionRef{SessionID: "session-42"})
			if err != nil {
				t.Fatalf("build %s: %v", method, err)
			}
			back := tripMessage(t, request)
			if back.Method != method {
				t.Fatalf("method = %q, want %q", back.Method, method)
			}
			var ref SessionRef
			if err := back.Into(&ref); err != nil {
				t.Fatalf("read %s: %v", method, err)
			}
			if ref.SessionID != "session-42" {
				t.Fatalf("session id = %q", ref.SessionID)
			}
		})
	}
}

func TestCloseRoundTrip(t *testing.T) {
	request, err := Request("4", MethodClose, Empty{})
	if err != nil {
		t.Fatalf("build close: %v", err)
	}
	back := tripMessage(t, request)
	if back.Kind != MessageRequest || back.Method != MethodClose {
		t.Fatalf("close came back as %+v", back)
	}
	answer := tripMessage(t, mustResult(t, "4", Empty{}))
	if answer.Kind != MessageResponse || answer.ID != "4" || answer.Fault() != nil {
		t.Fatalf("close answer came back as %+v", answer)
	}
}

// An older or terser peer sends no body at all, not even `{}`.
func TestAbsentBodyIsNotAnError(t *testing.T) {
	back := tripMessage(t, Message{Kind: MessageRequest, ID: "5", Method: MethodInitialize})
	var result InitializeResult
	if err := back.Into(&result); err != nil {
		t.Fatalf("an absent body errored: %v", err)
	}
	if result.Interruptible != nil {
		t.Fatal("an absent body invented a value")
	}
}

func TestEventFramesAddressTheirStream(t *testing.T) {
	event, err := StreamEvent("9", Event{Type: EventText, Text: "hello"})
	if err != nil {
		t.Fatalf("build event: %v", err)
	}
	back := tripMessage(t, event)
	if back.Kind != MessageEvent || back.ID != "9" || back.SessionID != "" || back.End {
		t.Fatalf("stream event came back as %+v", back)
	}
	var payload Event
	if err := back.Into(&payload); err != nil {
		t.Fatalf("read event: %v", err)
	}
	if payload.Type != EventText || payload.Text != "hello" {
		t.Fatalf("event came back as %+v", payload)
	}

	end := tripMessage(t, StreamEnd("9"))
	if !end.End || end.ID != "9" || len(end.Body) != 0 {
		t.Fatalf("stream end came back as %+v", end)
	}
}

func TestBackgroundEventIsAddressedBySession(t *testing.T) {
	event, err := BackgroundEvent("session-42", Event{Type: EventComplete, StopReason: "end_turn"})
	if err != nil {
		t.Fatalf("build background event: %v", err)
	}
	back := tripMessage(t, event)
	if back.ID != "" {
		t.Fatalf("background event claimed request id %q", back.ID)
	}
	if back.SessionID != "session-42" {
		t.Fatalf("background event session = %q", back.SessionID)
	}
}

func TestPermissionAnswerAndChunkRoundTrip(t *testing.T) {
	answer, err := PermissionAnswer(PermissionResponse{ID: "p1", OptionID: agent.PermissionAllow, Note: "go ahead"})
	if err != nil {
		t.Fatalf("build permission answer: %v", err)
	}
	back := tripMessage(t, answer)
	if back.Kind != MessagePermission || back.ID != "p1" {
		t.Fatalf("permission answer came back as %+v", back)
	}
	var resp PermissionResponse
	if err := back.Into(&resp); err != nil {
		t.Fatalf("read permission answer: %v", err)
	}
	if allowed, note := resp.Approval(); !allowed || note != "go ahead" {
		t.Fatalf("approval = (%v, %q)", allowed, note)
	}

	chunk, err := Chunk(TransferChunk{TransferID: "t1", Seq: 3, Data: []byte("bytes"), Last: true})
	if err != nil {
		t.Fatalf("build chunk: %v", err)
	}
	var got TransferChunk
	if err := tripMessage(t, chunk).Into(&got); err != nil {
		t.Fatalf("read chunk: %v", err)
	}
	if got.TransferID != "t1" || got.Seq != 3 || string(got.Data) != "bytes" || !got.Last {
		t.Fatalf("chunk came back as %+v", got)
	}
}

func TestDecodeMessageRejectsAKindlessFrame(t *testing.T) {
	if _, err := DecodeMessage([]byte(`{"id":"1"}`)); err == nil {
		t.Fatal("a message with no kind decoded cleanly")
	}
}

func mustResult(t *testing.T, id string, body any) Message {
	t.Helper()
	m, err := Result(id, body)
	if err != nil {
		t.Fatalf("build result: %v", err)
	}
	return m
}
