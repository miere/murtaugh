package agentwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/voocel/litellm/providers"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agent/acp"
	"github.com/miere/murtaugh/internal/llm"
)

func trip(t *testing.T, w Event) Event {
	t.Helper()
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal wire event: %v", err)
	}
	var back Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal wire event: %v", err)
	}
	return back
}

func roundTrip(t *testing.T, enc *Encoder, dec *Decoder, ev agent.Event) (agent.Event, *Transfer, *PendingDecision) {
	t.Helper()
	w, transfer, err := enc.Encode(ev)
	if err != nil {
		t.Fatalf("Encode(%s): %v", ev.Type, err)
	}
	back, pending, err := dec.Decode(context.Background(), trip(t, w))
	if err != nil {
		t.Fatalf("Decode(%s): %v", w.Type, err)
	}
	return back, transfer, pending
}

func TestRoundTripTextKinds(t *testing.T) {
	cases := []struct {
		name string
		ev   agent.Event
	}{
		{"text", agent.Event{Type: agent.EventText, Text: "hello, world"}},
		{"status", agent.Event{Type: agent.EventStatus, Text: "still working…"}},
		{"complete", agent.Event{Type: agent.EventComplete, StopReason: "max_tokens"}},
		{"complete without a stop reason", agent.Event{Type: agent.EventComplete}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc, dec := NewEncoder(), NewDecoder(nil)
			back, transfer, pending := roundTrip(t, enc, dec, tc.ev)
			if transfer != nil || pending != nil {
				t.Fatalf("%s produced a side channel; want none", tc.ev.Type)
			}
			if back != tc.ev {
				t.Errorf("round trip = %+v, want %+v", back, tc.ev)
			}
		})
	}
}

func TestRoundTripTask(t *testing.T) {
	cases := []struct {
		name string
		task agent.TaskEvent
	}{
		{"tool", agent.TaskEvent{
			ID:          "call_7",
			Title:       "terminal",
			Status:      agent.TaskStatusInProgress,
			Description: "running the migration",
			Output:      "applied 3 migrations",
			Kind:        agent.TaskKindTool,
		}},
		{"plan entry", agent.TaskEvent{
			ID:     "plan-2",
			Title:  "Write the round-trip test",
			Status: agent.TaskStatusPending,
			Kind:   agent.TaskKindPlan,
		}},
		{"failed with a note", agent.TaskEvent{
			ID:     "call_9",
			Title:  "attach",
			Status: agent.TaskStatusFailed,
			Output: "Denied by the user.",
		}},
		{"cancelled", agent.TaskEvent{ID: "call_1", Title: "sleep", Status: agent.TaskStatusCancelled}},
		{"complete", agent.TaskEvent{ID: "call_2", Title: "read", Status: agent.TaskStatusComplete}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc, dec := NewEncoder(), NewDecoder(nil)
			back, _, _ := roundTrip(t, enc, dec, agent.Event{Type: agent.EventTask, Task: &tc.task})
			if back.Task == nil {
				t.Fatal("round trip dropped the task")
			}
			if *back.Task != tc.task {
				t.Errorf("round trip = %+v, want %+v", *back.Task, tc.task)
			}
		})
	}
}

const geminiOverloadBody = `{"error":{"code":503,"message":"This model is currently experiencing high demand. Spikes in demand are usually temporary. Please try again later.","status":"UNAVAILABLE"}}`

func TestRoundTripErrorIdentity(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantKind ErrorKind
		check    func(t *testing.T, decoded error)
	}{
		{
			name:     "bare cancellation from the native loop",
			err:      context.Canceled,
			wantKind: ErrorCancelled,
			check: func(t *testing.T, decoded error) {
				if !errors.Is(decoded, context.Canceled) {
					t.Error("errors.Is(decoded, context.Canceled) = false; the interrupt would render as a failure card")
				}
			},
		},
		{
			name:     "wrapped cancellation from the ACP client",
			err:      fmt.Errorf("acp: turn interrupted: %w", context.Canceled),
			wantKind: ErrorCancelled,
			check: func(t *testing.T, decoded error) {
				if !errors.Is(decoded, context.Canceled) {
					t.Error("errors.Is(decoded, context.Canceled) = false")
				}
				if got := decoded.Error(); got != "acp: turn interrupted: context canceled" {
					t.Errorf("message = %q, want the producer's prose verbatim", got)
				}
			},
		},
		{
			name:     "wrapped cancellation from the claude_code client",
			err:      fmt.Errorf("claudecode: turn interrupted: %w", context.Canceled),
			wantKind: ErrorCancelled,
			check: func(t *testing.T, decoded error) {
				if !errors.Is(decoded, context.Canceled) {
					t.Error("errors.Is(decoded, context.Canceled) = false")
				}
				if !strings.HasPrefix(decoded.Error(), "claudecode: turn interrupted") {
					t.Errorf("message = %q, want the producer's prose", decoded.Error())
				}
			},
		},
		{
			name:     "tool ceiling with its diagnostic detail",
			err:      fmt.Errorf("%w: %q ran for 5m0s with no result", agent.ErrToolCeiling, "terminal"),
			wantKind: ErrorToolCeiling,
			check: func(t *testing.T, decoded error) {
				if !errors.Is(decoded, agent.ErrToolCeiling) {
					t.Error("errors.Is(decoded, agent.ErrToolCeiling) = false; the wedged session would not be dropped")
				}
				if !strings.Contains(decoded.Error(), `"terminal" ran for 5m0s`) {
					t.Errorf("message = %q, want the tool name and elapsed time", decoded.Error())
				}
			},
		},
		{
			name:     "provider failure classified by the backend",
			err:      llm.CarryFailure(fmt.Errorf("native: provider stream: %w", providers.NewHTTPError("gemini", 503, geminiOverloadBody))),
			wantKind: ErrorProvider,
			check: func(t *testing.T, decoded error) {
				failure, ok := llm.Classify(decoded)
				if !ok {
					t.Fatal("llm.Classify(decoded) ok = false; the alert card would lose the provider vocabulary")
				}
				if got, want := failure.String(), "Gemini is overloaded (503)"; got != want {
					t.Errorf("Failure.String() = %q, want %q", got, want)
				}
				if failure.Kind != llm.FailureOverloaded || !failure.Retryable {
					t.Errorf("Failure = %+v, want an overloaded, retryable Gemini failure", failure)
				}
				if !strings.Contains(failure.Message, "experiencing high demand") {
					t.Errorf("Failure.Message = %q, want the provider's own sentence", failure.Message)
				}
			},
		},
		{
			name:     "a credential failure the node has asked its owner to fix",
			err:      fmt.Errorf("%w: %w", agent.ErrCredentialRejected, errors.New("claudecode: session failed: API Error: 401 Invalid API key · Please run /login")),
			wantKind: ErrorCredential,
			check: func(t *testing.T, decoded error) {
				if !errors.Is(decoded, agent.ErrCredentialRejected) {
					t.Error("errors.Is(decoded, agent.ErrCredentialRejected) = false; the gateway would not tell the user the owner was asked")
				}
				if !strings.Contains(decoded.Error(), "401 Invalid API key") {
					t.Errorf("message = %q, want the CLI's own words", decoded.Error())
				}
			},
		},
		{
			name:     "a credential failure the node did not mark stays the node's business",
			err:      fmt.Errorf("claudecode: session failed: %w", errors.New("API Error: 401 Invalid API key · Please run /login")),
			wantKind: ErrorUnknown,
			check: func(t *testing.T, decoded error) {
				if errors.Is(decoded, agent.ErrCredentialRejected) {
					t.Error("the wire invented a credential failure the node never named")
				}
			},
		},
		{
			name:     "ACP RPC fault keeps its numeric code",
			err:      fmt.Errorf("acp: prompt: %w", &acp.RPCError{Method: "session/cancel", Code: -32601, Message: "method not found"}),
			wantKind: ErrorRPC,
			check: func(t *testing.T, decoded error) {
				var fault RPCFaulter
				if !errors.As(decoded, &fault) {
					t.Fatal("errors.As(decoded, &RPCFaulter) = false; the JSON-RPC code was flattened to prose")
				}
				method, code := fault.RPCFault()
				if method != "session/cancel" || code != -32601 {
					t.Errorf("RPCFault() = (%q, %d), want (\"session/cancel\", -32601)", method, code)
				}
			},
		},
		{
			name:     "an error with no discriminant keeps its text",
			err:      errors.New("start Slack stream: channel_type_not_supported"),
			wantKind: ErrorUnknown,
			check: func(t *testing.T, decoded error) {
				if errors.Is(decoded, context.Canceled) || errors.Is(decoded, agent.ErrToolCeiling) {
					t.Error("an unclassified error acquired a sentinel it never had")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc, dec := NewEncoder(), NewDecoder(nil)
			w, _, err := enc.Encode(agent.Event{Type: agent.EventError, Error: tc.err})
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if w.Error == nil {
				t.Fatal("encoded event carries no error")
			}
			if w.Error.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", w.Error.Kind, tc.wantKind)
			}

			back, _, err := dec.Decode(context.Background(), trip(t, w))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if back.Type != agent.EventError {
				t.Fatalf("Type = %q, want %q", back.Type, agent.EventError)
			}
			if back.Error == nil {
				t.Fatal("round trip dropped the error")
			}
			if got, want := back.Error.Error(), tc.err.Error(); got != want {
				t.Errorf("message = %q, want %q", got, want)
			}
			tc.check(t, back.Error)
		})
	}
}

func TestRoundTripErrorNil(t *testing.T) {
	enc, dec := NewEncoder(), NewDecoder(nil)
	back, _, _ := roundTrip(t, enc, dec, agent.Event{Type: agent.EventError})
	if back.Error != nil {
		t.Errorf("Error = %v, want nil", back.Error)
	}
}

func TestRoundTripPermissionRequest(t *testing.T) {
	req := agent.PermissionRequest{
		SessionID: "sess-1",
		ToolKind:  "execute",
		ToolTitle: "rm -rf ./build",
		Options: []agent.PermissionOption{
			{ID: "opt-a", Name: "Allow once", Kind: "allow_once"},
			{ID: "opt-b", Name: "Always allow", Kind: "allow_always"},
			{ID: "opt-c", Name: "Reject", Kind: "reject_once"},
		},
	}
	enc, dec := NewEncoder(), NewDecoder(nil)
	back, _, pending := roundTrip(t, enc, dec, agent.Event{
		Type:       agent.EventPermission,
		Permission: &agent.PermissionPrompt{Request: req, Decision: make(chan string, 1)},
	})
	if back.Permission == nil {
		t.Fatal("round trip dropped the permission prompt")
	}
	got := back.Permission.Request
	if got.SessionID != req.SessionID || got.ToolKind != req.ToolKind || got.ToolTitle != req.ToolTitle {
		t.Errorf("request = %+v, want %+v", got, req)
	}
	if len(got.Options) != len(req.Options) {
		t.Fatalf("options = %d, want %d", len(got.Options), len(req.Options))
	}
	for i := range req.Options {
		if got.Options[i] != req.Options[i] {
			t.Errorf("option %d = %+v, want %+v", i, got.Options[i], req.Options[i])
		}
	}
	if got.PolicyOwned {
		t.Error("PolicyOwned = true, want false for an options-carrying request")
	}
	if back.Permission.Decision == nil {
		t.Fatal("decoded prompt has no decision channel for the consumer to answer on")
	}
	if pending == nil || pending.ID == "" {
		t.Fatal("Decode returned no pending decision to correlate the answer with")
	}
}

func TestPermissionResponseReachesTheBlockedBackend(t *testing.T) {
	cases := []struct {
		name     string
		optionID string
	}{
		{"an agent-declared option id", "opt-b"},
		{"a policy-owned allow", agent.PermissionAllow},
		{"a policy-owned deny", agent.PermissionDeny},
		{"nobody chose", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc, dec := NewEncoder(), NewDecoder(nil)
			decision := make(chan string, 1)
			prompt := &agent.PermissionPrompt{
				Request:  agent.PermissionRequest{ToolKind: "terminal", ToolTitle: "ls", PolicyOwned: true},
				Decision: decision,
			}
			w, _, err := enc.Encode(agent.Event{Type: agent.EventPermission, Permission: prompt})
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			back, pending, err := dec.Decode(context.Background(), trip(t, w))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !back.Permission.Request.PolicyOwned {
				t.Error("PolicyOwned = false, want true")
			}
			if len(back.Permission.Request.Options) != 0 {
				t.Errorf("options = %+v, want none on a policy-owned request", back.Permission.Request.Options)
			}

			back.Permission.Decision <- tc.optionID
			resp := Response(pending.ID, <-pending.Decision)

			raw, err := json.Marshal(resp)
			if err != nil {
				t.Fatalf("marshal response: %v", err)
			}
			var backResp PermissionResponse
			if err := json.Unmarshal(raw, &backResp); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			if err := enc.Resolve(backResp); err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			select {
			case got := <-decision:
				if got != tc.optionID {
					t.Errorf("backend received %q, want %q", got, tc.optionID)
				}
			default:
				t.Fatal("the backend is still blocked; the answer never arrived")
			}
			if n := enc.Pending(); n != 0 {
				t.Errorf("pending = %d after resolving, want 0", n)
			}
		})
	}
}

func TestPermissionResolveRejectsAnUnknownID(t *testing.T) {
	enc := NewEncoder()
	if err := enc.Resolve(Response("no-such-request", agent.PermissionAllow)); err == nil {
		t.Fatal("Resolve of an unknown id = nil, want an error")
	}
}

func TestPermissionAbandonReleasesThePending(t *testing.T) {
	enc := NewEncoder()
	w, _, err := enc.Encode(agent.Event{
		Type:       agent.EventPermission,
		Permission: &agent.PermissionPrompt{Decision: make(chan string, 1)},
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if enc.Pending() != 1 {
		t.Fatalf("pending = %d, want 1", enc.Pending())
	}
	enc.Abandon(w.Permission.ID)
	if enc.Pending() != 0 {
		t.Errorf("pending = %d after Abandon, want 0", enc.Pending())
	}
}

func TestNativeApprovalRidesTheSameFrame(t *testing.T) {
	req := NativeApproval("req-1", "terminal", "rm -rf ./build")
	if req.Gate != GateTool {
		t.Errorf("Gate = %q, want %q", req.Gate, GateTool)
	}
	if !req.PolicyOwned {
		t.Error("PolicyOwned = false; the gateway owns the whole decision for a native approval")
	}

	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var back PermissionRequest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if back.ID != req.ID || back.Gate != req.Gate || back.ToolKind != req.ToolKind ||
		back.ToolTitle != req.ToolTitle || back.PolicyOwned != req.PolicyOwned || len(back.Options) != 0 {
		t.Errorf("round trip = %+v, want %+v", back, req)
	}

	cases := []struct {
		name     string
		resp     PermissionResponse
		allowed  bool
		wantNote string
	}{
		{"approved", Response("req-1", agent.PermissionAllow), true, ""},
		{"denied with the note the model is told", PermissionResponse{
			ID:       "req-1",
			OptionID: agent.PermissionDeny,
			Note:     "Denied by the user. The action was not run; do not retry it without their go-ahead.",
		}, false, "Denied by the user. The action was not run; do not retry it without their go-ahead."},
		{"nobody answered", PermissionResponse{
			ID:   "req-1",
			Note: "Skipped: no approval received in time. The action was not run — ask again if it is still needed.",
		}, false, "Skipped: no approval received in time. The action was not run — ask again if it is still needed."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.resp)
			if err != nil {
				t.Fatalf("marshal response: %v", err)
			}
			var back PermissionResponse
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			allowed, note := back.Approval()
			if allowed != tc.allowed || note != tc.wantNote {
				t.Errorf("Approval() = (%v, %q), want (%v, %q)", allowed, note, tc.allowed, tc.wantNote)
			}
		})
	}
}

type memDeliverer struct {
	bytesByID map[string][]byte
	dir       string
	seen      Attachment
}

func (d *memDeliverer) DeliverAttachment(_ context.Context, a Attachment) (string, []byte, error) {
	d.seen = a
	body, ok := d.bytesByID[a.TransferID]
	if !ok {
		return "", nil, fmt.Errorf("no transfer %q", a.TransferID)
	}
	if int64(len(body)) != a.Size {
		return "", nil, fmt.Errorf("transfer %q delivered %d bytes, declared %d", a.TransferID, len(body), a.Size)
	}
	if d.dir == "" {
		return "", body, nil
	}
	path := filepath.Join(d.dir, a.TransferID)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", nil, err
	}
	return path, nil, nil
}

func drain(t *testing.T, d *memDeliverer, transfer *Transfer) {
	t.Helper()
	if transfer == nil {
		t.Fatal("encoding an attachment produced no transfer")
	}
	defer transfer.Body.Close()
	body, err := io.ReadAll(transfer.Body)
	if err != nil {
		t.Fatalf("read transfer: %v", err)
	}
	if int64(len(body)) != transfer.Size {
		t.Fatalf("transfer carried %d bytes, declared %d", len(body), transfer.Size)
	}
	if d.bytesByID == nil {
		d.bytesByID = map[string][]byte{}
	}
	d.bytesByID[transfer.ID] = body
}

func assertAttachmentFrameCarriesMetadataOnly(t *testing.T, raw []byte) {
	t.Helper()
	var frame struct {
		Attachment map[string]json.RawMessage `json:"attachment"`
	}
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("unmarshal event frame: %v", err)
	}
	if len(frame.Attachment) == 0 {
		t.Fatal("event frame carries no attachment object")
	}
	allowed := map[string]bool{
		"filename": true, "title": true, "comment": true, "mimetype": true,
		"size": true, "transfer_id": true,
	}
	for field := range frame.Attachment {
		if !allowed[field] {
			t.Errorf("attachment frame carries field %q; the bytes ride a side transfer, "+
				"so anything beyond the metadata and the reference has to be argued for", field)
		}
	}
	if _, ok := frame.Attachment["transfer_id"]; !ok {
		t.Error("attachment frame carries no transfer_id; there is nothing to correlate the bytes with")
	}
}

func TestRoundTripAttachmentFromACPBytes(t *testing.T) {
	want := bytes.Repeat([]byte("a,b,c\n1,2,3\n"), 512)
	a := &agent.AttachmentEvent{
		Filename: "report.csv",
		Title:    "Weekly report",
		Comment:  "here it is",
		Mimetype: "text/csv",
		Data:     want,
	}
	enc := NewEncoder()
	w, transfer, err := enc.Encode(agent.Event{Type: agent.EventAttachment, Attachment: a})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if w.Attachment.TransferID == "" {
		t.Fatal("attachment crossed the wire with no transfer id")
	}
	if w.Attachment.Size != int64(len(want)) {
		t.Errorf("Size = %d, want %d", w.Attachment.Size, len(want))
	}
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(raw) > 512 {
		t.Errorf("event frame is %d bytes for a %d-byte payload; the payload leaked into it", len(raw), len(want))
	}
	assertAttachmentFrameCarriesMetadataOnly(t, raw)

	deliverer := &memDeliverer{}
	drain(t, deliverer, transfer)

	back, _, err := NewDecoder(deliverer).Decode(context.Background(), trip(t, w))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got := back.Attachment
	if got == nil {
		t.Fatal("round trip dropped the attachment")
	}
	if got.Filename != a.Filename || got.Title != a.Title || got.Comment != a.Comment || got.Mimetype != a.Mimetype {
		t.Errorf("metadata = %+v, want %+v", got, a)
	}
	if string(got.Data) != string(want) {
		t.Errorf("data = %q, want %q", got.Data, want)
	}
	if deliverer.seen.Size != int64(len(want)) {
		t.Errorf("deliverer saw Size = %d, want %d", deliverer.seen.Size, len(want))
	}
}

func TestRoundTripAttachmentFromANativeToolPath(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "build.log")
	want := []byte("BUILD SUCCESSFUL in 4s\n")
	if err := os.WriteFile(src, want, 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	a := &agent.AttachmentEvent{Filename: "build.log", Title: "Build log", Path: src}

	enc := NewEncoder()
	w, transfer, err := enc.Encode(agent.Event{Type: agent.EventAttachment, Attachment: a})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if w.Attachment.Size != int64(len(want)) {
		t.Errorf("Size = %d, want %d", w.Attachment.Size, len(want))
	}
	if strings.Contains(string(mustJSON(t, w)), src) {
		t.Error("the node's local path crossed the wire; it means nothing on the gateway")
	}
	assertAttachmentFrameCarriesMetadataOnly(t, mustJSON(t, w))

	deliverer := &memDeliverer{dir: t.TempDir()}
	drain(t, deliverer, transfer)

	back, _, err := NewDecoder(deliverer).Decode(context.Background(), trip(t, w))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if back.Attachment.Path == "" {
		t.Fatal("decoded attachment has no local path")
	}
	if back.Attachment.Path == src {
		t.Error("decoded attachment reuses the node's path")
	}
	got, err := os.ReadFile(back.Attachment.Path)
	if err != nil {
		t.Fatalf("read delivered file: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("delivered %q, want %q", got, want)
	}
}

func TestDecodeAttachmentWithoutADelivererFails(t *testing.T) {
	enc := NewEncoder()
	w, transfer, err := enc.Encode(agent.Event{
		Type:       agent.EventAttachment,
		Attachment: &agent.AttachmentEvent{Filename: "x.txt", Data: []byte("x")},
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	transfer.Body.Close()
	if _, _, err := NewDecoder(nil).Decode(context.Background(), w); err == nil {
		t.Fatal("Decode with no deliverer = nil error; a silently dropped file is exactly what this must not do")
	}
}

func TestEncodeAttachmentWithoutBytesFails(t *testing.T) {
	enc := NewEncoder()
	if _, _, err := enc.Encode(agent.Event{
		Type:       agent.EventAttachment,
		Attachment: &agent.AttachmentEvent{Filename: "x.txt"},
	}); err == nil {
		t.Fatal("Encode of an attachment with neither data nor a path = nil error")
	}
}

func TestTransferChunkRoundTrip(t *testing.T) {
	cases := []TransferChunk{
		{TransferID: "t-1", Seq: 0, Data: []byte{0x00, 0x01, 0xff}},
		{TransferID: "t-1", Seq: 1, Data: []byte("tail"), Last: true},
		{TransferID: "t-1", Seq: 2, Error: "read attachment: input/output error"},
	}
	for _, c := range cases {
		var back TransferChunk
		if err := json.Unmarshal(mustJSON(t, c), &back); err != nil {
			t.Fatalf("unmarshal chunk: %v", err)
		}
		if back.TransferID != c.TransferID || back.Seq != c.Seq || back.Last != c.Last || back.Error != c.Error {
			t.Errorf("chunk = %+v, want %+v", back, c)
		}
		if string(back.Data) != string(c.Data) {
			t.Errorf("data = %q, want %q", back.Data, c.Data)
		}
	}
	if MaxTransferChunkBytes*4/3 >= maxFrameBytes {
		t.Errorf("a base64-encoded %d-byte chunk does not fit the %d-byte frame ceiling", MaxTransferChunkBytes, maxFrameBytes)
	}
}

// The kinds are read from internal/agent's source rather than restated here, so this guard
// cannot drift into agreeing with a stale copy.
func TestEncodeCoversEveryEventKind(t *testing.T) {
	signIn := &agent.SignInPrompt{Answer: make(chan agent.DisplayAnswer, 2)}
	covered := map[agent.EventType]struct {
		ev   agent.Event
		want EventType
	}{
		agent.EventText:     {agent.Event{Type: agent.EventText, Text: "x"}, EventText},
		agent.EventStatus:   {agent.Event{Type: agent.EventStatus, Text: "x"}, EventStatus},
		agent.EventComplete: {agent.Event{Type: agent.EventComplete}, EventComplete},
		agent.EventError:    {agent.Event{Type: agent.EventError, Error: errors.New("x")}, EventError},
		agent.EventTask:     {agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{ID: "1"}}, EventTask},
		agent.EventAttachment: {agent.Event{
			Type:       agent.EventAttachment,
			Attachment: &agent.AttachmentEvent{Filename: "x", Data: []byte("x")},
		}, EventAttachment},
		agent.EventPermission: {agent.Event{
			Type:       agent.EventPermission,
			Permission: &agent.PermissionPrompt{Decision: make(chan string, 1)},
		}, EventPermission},
		agent.EventQuestion: {agent.Event{
			Type:     agent.EventQuestion,
			Question: &agent.QuestionPrompt{Answer: make(chan agent.DisplayAnswer, 1)},
		}, EventQuestion},
		agent.EventPlan: {agent.Event{
			Type: agent.EventPlan,
			Plan: &agent.PlanPrompt{Answer: make(chan agent.DisplayAnswer, 1)},
		}, EventPlan},
		agent.EventSignIn:        {agent.Event{Type: agent.EventSignIn, SignIn: signIn}, EventSignIn},
		agent.EventSignInSettled: {agent.Event{Type: agent.EventSignInSettled, SignInSettled: &agent.SignInSettled{Prompt: signIn, State: agent.SignInSuccess}}, EventSignInSettled},
	}

	declared := agentEventKinds(t)
	enc := NewEncoder()
	if _, _, err := enc.Encode(covered[agent.EventSignIn].ev); err != nil {
		t.Fatalf("Encode(sign_in): %v", err)
	}
	for _, kind := range declared {
		tc, ok := covered[kind]
		if !ok {
			t.Errorf("internal/agent declares event kind %q and this file does not cover it; "+
				"teach Encode/Decode about it and add it here, or a runtime node drops it", kind)
			continue
		}
		w, transfer, err := enc.Encode(tc.ev)
		if err != nil {
			t.Errorf("Encode(%s): %v", kind, err)
			continue
		}
		if transfer != nil {
			transfer.Body.Close()
		}
		if w.Type != tc.want {
			t.Errorf("Encode(%s).Type = %q, want %q", kind, w.Type, tc.want)
		}
	}
	for kind := range covered {
		if !slices.Contains(declared, kind) {
			t.Errorf("this file covers event kind %q, which internal/agent no longer declares", kind)
		}
	}
}

func agentEventKinds(t *testing.T) []agent.EventType {
	t.Helper()
	const dir = "../agent"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var kinds []agent.EventType
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			typeName := ""
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if ident, ok := value.Type.(*ast.Ident); ok {
					typeName = ident.Name
				} else if value.Type != nil || len(value.Values) > 0 {
					typeName = ""
				}
				if typeName != "EventType" {
					continue
				}
				for _, v := range value.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s: EventType constant is not a string literal; teach this guard about it", name)
					}
					s, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("%s: unquote %s: %v", name, lit.Value, err)
					}
					kinds = append(kinds, agent.EventType(s))
				}
			}
		}
	}
	if len(kinds) == 0 {
		t.Fatalf("found no agent.EventType constants under %s; the guard is reading the wrong thing", dir)
	}
	return kinds
}

func TestEncodeRejectsAnUnknownKind(t *testing.T) {
	if _, _, err := NewEncoder().Encode(agent.Event{Type: "telepathy"}); err == nil {
		t.Fatal("Encode of an unknown kind = nil error; a new kind must fail loudly, not vanish")
	}
}

func TestDecodeRejectsAnUnknownKind(t *testing.T) {
	if _, _, err := NewDecoder(nil).Decode(context.Background(), Event{Type: "telepathy"}); err == nil {
		t.Fatal("Decode of an unknown kind = nil error")
	}
}

func TestWireVocabularyIsIndependent(t *testing.T) {
	for _, tc := range []struct {
		got  EventType
		want string
	}{
		{EventText, "text"},
		{EventStatus, "status"},
		{EventComplete, "complete"},
		{EventError, "error"},
		{EventTask, "task"},
		{EventAttachment, "attachment"},
		{EventPermission, "permission"},
		{EventQuestion, "question"},
		{EventPlan, "plan"},
		{EventSignIn, "sign_in"},
		{EventSignInSettled, "sign_in_settled"},
	} {
		if string(tc.got) != tc.want {
			t.Errorf("wire kind = %q, want %q", tc.got, tc.want)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func answerTrip(t *testing.T, enc *Encoder, id string, a agent.DisplayAnswer) {
	t.Helper()
	msg, err := AnswerDisplay(EncodeDisplayAnswer(id, a))
	if err != nil {
		t.Fatalf("AnswerDisplay: %v", err)
	}
	raw, err := msg.Encode()
	if err != nil {
		t.Fatalf("encode frame: %v", err)
	}
	back, err := DecodeMessage(raw)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if back.Kind != MessageAnswer || back.ID != id {
		t.Fatalf("the answer frame came back as kind %q id %q", back.Kind, back.ID)
	}
	var answer DisplayAnswer
	if err := back.Into(&answer); err != nil {
		t.Fatalf("read answer: %v", err)
	}
	if err := enc.Answer(answer); err != nil {
		t.Fatalf("Answer: %v", err)
	}
}

func TestAQuestionRoundTripsAndItsAnswerReachesTheTool(t *testing.T) {
	enc, dec := NewEncoder(), NewDecoder(nil)
	waiting := make(chan agent.DisplayAnswer, 1)
	request := agent.QuestionRequest{Title: "Deploy", Questions: []agent.Question{
		{Key: "q0", Header: "Env", Question: "Where?", Options: []agent.QuestionOption{{Label: "Staging", Description: "safe"}, {Label: "Production"}}},
		{Key: "q1", Question: "Regions?", MultiSelect: true, Options: []agent.QuestionOption{{Label: "US"}, {Label: "EU"}}},
	}}

	back, _, pending := roundTrip(t, enc, dec, agent.Event{
		Type:     agent.EventQuestion,
		Question: &agent.QuestionPrompt{Request: request, Answer: waiting},
	})
	if back.Type != agent.EventQuestion || back.Question == nil {
		t.Fatalf("decoded %+v, want a question", back)
	}
	if !reflect.DeepEqual(back.Question.Request, request) {
		t.Fatalf("the question changed crossing the wire:\n got %+v\nwant %+v", back.Question.Request, request)
	}
	if pending == nil || pending.ID == "" {
		t.Fatalf("Decode returned %+v; the gateway has nothing to answer under", pending)
	}

	back.Question.Answer <- agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Answers: map[string][]string{"q0": {"Production"}, "q1": {"US", "EU"}}, UserID: "U1"}
	answerTrip(t, enc, pending.ID, <-back.Question.Answer)

	select {
	case got := <-waiting:
		if got.Outcome != agent.DisplayAnswered || got.UserID != "U1" || len(got.Answers["q1"]) != 2 {
			t.Fatalf("the tool received %+v", got)
		}
	default:
		t.Fatal("the tool is still waiting; the answer never arrived")
	}
	if n := enc.Pending(); n != 0 {
		t.Errorf("pending = %d after answering, want 0", n)
	}
}

func TestAPlanRoundTripsAndItsAnswerReachesTheTool(t *testing.T) {
	enc, dec := NewEncoder(), NewDecoder(nil)
	waiting := make(chan agent.DisplayAnswer, 1)
	request := agent.PlanRequest{Title: "Plan", Plan: "1. back up\n2. migrate"}

	back, _, pending := roundTrip(t, enc, dec, agent.Event{
		Type: agent.EventPlan,
		Plan: &agent.PlanPrompt{Request: request, Answer: waiting},
	})
	if back.Type != agent.EventPlan || back.Plan == nil || back.Plan.Request != request {
		t.Fatalf("decoded %+v, want the plan unchanged", back)
	}

	back.Plan.Answer <- agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Choice: agent.PlanRevise}
	answerTrip(t, enc, pending.ID, <-back.Plan.Answer)

	select {
	case got := <-waiting:
		if got.Outcome != agent.DisplayAnswered || got.Choice != agent.PlanRevise {
			t.Fatalf("the tool received %+v", got)
		}
	default:
		t.Fatal("the tool is still waiting; the answer never arrived")
	}
}

func TestADisplayAnswerForAnUnknownRequestIsAnError(t *testing.T) {
	if err := NewEncoder().Answer(DisplayAnswer{ID: "no-such-request"}); err == nil {
		t.Fatal("Answer of an unknown id = nil, want an error")
	}
}

// The gateway alone decides where a card is drawn, so every field a display
// request may carry is listed here and a new one fails until someone reviews it.
func TestDisplayRequestsCarryOnlyReviewedFields(t *testing.T) {
	want := map[string][]string{
		"QuestionRequest": {"ID id", "Title title", "Questions questions"},
		"Question":        {"Key key", "Header header", "Question question", "Options options", "MultiSelect multi_select"},
		"QuestionOption":  {"Label label", "Description description"},
		"PlanRequest":     {"ID id", "Title title", "Plan plan"},
		"SignInRequest":   {"ID id", "Tool tool", "Profile profile", "URL url", "NeedsCode needs_code", "Command command"},
		"SignInSettled":   {"ID id", "State state", "Reason reason", "URL url"},
	}
	got := map[string][]string{}
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct {
			return
		}
		if _, seen := got[typ.Name()]; seen {
			return
		}
		got[typ.Name()] = []string{}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			tag, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			got[typ.Name()] = append(got[typ.Name()], field.Name+" "+tag)
			walk(field.Type)
		}
	}
	walk(reflect.TypeOf(QuestionRequest{}))
	walk(reflect.TypeOf(PlanRequest{}))
	walk(reflect.TypeOf(SignInRequest{}))
	walk(reflect.TypeOf(SignInSettled{}))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("display request fields changed; review that none names a destination or carries environment, then update this list.\n got %v\nwant %v", got, want)
	}

	var fields []string
	typ := reflect.TypeOf(agent.SignInRequest{})
	for i := 0; i < typ.NumField(); i++ {
		fields = append(fields, typ.Field(i).Name)
	}
	if want := []string{"Tool", "Profile", "URL", "NeedsCode", "Command"}; !reflect.DeepEqual(fields, want) {
		t.Fatalf("agent.SignInRequest fields changed; review that none names a destination or carries environment.\n got %v\nwant %v", fields, want)
	}
}

func TestASignInRoundTripsAndStaysOpenUntilItSettles(t *testing.T) {
	enc, dec := NewEncoder(), NewDecoder(nil)
	onNode := &agent.SignInPrompt{
		Request: agent.SignInRequest{Tool: "gcp-mcp", Profile: "gcloud", URL: "https://accounts.example.com/o/oauth2?x=1", NeedsCode: true},
		Answer:  make(chan agent.DisplayAnswer, 2),
	}

	back, _, pending := roundTrip(t, enc, dec, agent.Event{Type: agent.EventSignIn, SignIn: onNode})
	if back.Type != agent.EventSignIn || back.SignIn == nil || back.SignIn.Request != onNode.Request {
		t.Fatalf("decoded %+v, want the sign-in unchanged", back)
	}
	if pending == nil || pending.ID == "" {
		t.Fatalf("Decode returned %+v; the gateway has nothing to answer under", pending)
	}

	answerTrip(t, enc, pending.ID, agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Code: "4/0Ab-code", UserID: "U1"})
	if got := <-onNode.Answer; got.Outcome != agent.DisplayAnswered || got.Code != "4/0Ab-code" {
		t.Fatalf("the node received %+v", got)
	}
	answerTrip(t, enc, pending.ID, agent.DisplayAnswer{Outcome: agent.DisplayDismissed})
	if got := <-onNode.Answer; got.Outcome != agent.DisplayDismissed {
		t.Fatalf("a cancel after the code reached the node as %+v", got)
	}

	working, _, _ := roundTrip(t, enc, dec, agent.Event{Type: agent.EventSignInSettled, SignInSettled: &agent.SignInSettled{Prompt: onNode, State: agent.SignInWorking}})
	if working.SignInSettled == nil || working.SignInSettled.Prompt != back.SignIn || working.SignInSettled.State != agent.SignInWorking {
		t.Fatalf("a progress update decoded as %+v", working.SignInSettled)
	}
	done, _, _ := roundTrip(t, enc, dec, agent.Event{Type: agent.EventSignInSettled, SignInSettled: &agent.SignInSettled{Prompt: onNode, State: agent.SignInFailed, Reason: "bad code"}})
	if done.SignInSettled == nil || done.SignInSettled.Prompt != back.SignIn || done.SignInSettled.State != agent.SignInFailed || done.SignInSettled.Reason != "bad code" {
		t.Fatalf("the terminal state decoded as %+v", done.SignInSettled)
	}

	if n := enc.Pending(); n != 0 {
		t.Errorf("pending = %d after the sign-in settled, want 0", n)
	}
	if err := enc.Answer(DisplayAnswer{ID: pending.ID, Outcome: string(agent.DisplayDismissed)}); err == nil {
		t.Error("a settled sign-in still accepted an answer")
	}
	if _, _, err := enc.Encode(agent.Event{Type: agent.EventSignInSettled, SignInSettled: &agent.SignInSettled{Prompt: onNode, State: agent.SignInFailed}}); err == nil {
		t.Error("a sign-in settled twice without an error")
	}
	if _, _, err := dec.Decode(context.Background(), Event{Type: EventSignInSettled, SignInSettled: &SignInSettled{ID: pending.ID, State: "failed"}}); err == nil {
		t.Error("the gateway decoded a settle for a sign-in it had already closed")
	}
}

func TestAnAbandonedSignInIsForgotten(t *testing.T) {
	enc := NewEncoder()
	prompt := &agent.SignInPrompt{Request: agent.SignInRequest{Tool: "x", Profile: "gcloud", URL: "https://x"}, Answer: make(chan agent.DisplayAnswer, 2)}
	w, _, err := enc.Encode(agent.Event{Type: agent.EventSignIn, SignIn: prompt})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	enc.Abandon(w.SignIn.ID)
	if n := enc.Pending(); n != 0 {
		t.Fatalf("pending = %d after abandoning the sign-in, want 0", n)
	}
	if _, _, err := enc.Encode(agent.Event{Type: agent.EventSignInSettled, SignInSettled: &agent.SignInSettled{Prompt: prompt, State: agent.SignInCancelled}}); err == nil {
		t.Fatal("an abandoned sign-in could still be settled")
	}
}

func TestACommandToApproveCrossesBeforeItsLink(t *testing.T) {
	enc, dec := NewEncoder(), NewDecoder(nil)
	onNode := &agent.SignInPrompt{
		Request: agent.SignInRequest{Tool: "vendor-mcp", Profile: "custom", Command: "vendor-cli login --headless", NeedsCode: true},
		Answer:  make(chan agent.DisplayAnswer, 2),
	}
	back, _, pending := roundTrip(t, enc, dec, agent.Event{Type: agent.EventSignIn, SignIn: onNode})
	if back.SignIn == nil || back.SignIn.Request != onNode.Request {
		t.Fatalf("decoded %+v, want the command unchanged and no link", back.SignIn)
	}

	answerTrip(t, enc, pending.ID, agent.DisplayAnswer{Outcome: agent.DisplayApproved, UserID: "UOWNER"})
	if got := <-onNode.Answer; got.Outcome != agent.DisplayApproved {
		t.Fatalf("the node received %+v", got)
	}
	ready, _, _ := roundTrip(t, enc, dec, agent.Event{Type: agent.EventSignInSettled, SignInSettled: &agent.SignInSettled{
		Prompt: onNode, State: agent.SignInReady, URL: "https://vendor.example.com/device",
	}})
	if ready.SignInSettled == nil || ready.SignInSettled.State != agent.SignInReady || ready.SignInSettled.URL != "https://vendor.example.com/device" {
		t.Fatalf("the link decoded as %+v", ready.SignInSettled)
	}
	if agent.SignInReady.Terminal() {
		t.Fatal("a sign-in whose link just arrived was treated as over")
	}
	answerTrip(t, enc, pending.ID, agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Code: "123-456"})
	if got := <-onNode.Answer; got.Code != "123-456" {
		t.Fatalf("the code after the link reached the node as %+v", got)
	}
}
