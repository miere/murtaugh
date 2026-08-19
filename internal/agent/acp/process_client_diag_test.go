package acp

import (
	"encoding/json"
	"testing"
)

func TestExtractStopReason(t *testing.T) {
	cases := map[string]string{
		`{"stopReason":"end_turn"}`:      "end_turn",
		`{"stop_reason":"max_tokens"}`:   "max_tokens",
		`{"stopReason":""}`:              "",
		`{"other":"x"}`:                  "",
		``:                               "",
		`{"stopReason":"refusal","x":1}`: "refusal",
	}
	for raw, want := range cases {
		if got := extractStopReason(json.RawMessage(raw)); got != want {
			t.Errorf("extractStopReason(%q) = %q, want %q", raw, got, want)
		}
	}
}

// TestIsCancelledStopReason pins which prompt results count as a cancelled turn
// rather than a completion — the ACP half of the backend-parity contract that a
// cancelled turn never reaches the empty-reply path.
func TestIsCancelledStopReason(t *testing.T) {
	cases := map[string]bool{
		"cancelled":  true,
		"canceled":   true, // an adapter using the American spelling
		"Cancelled":  true,
		" cancelled": true,
		"end_turn":   false,
		"refusal":    false,
		"max_tokens": false,
		"":           false,
	}
	for reason, want := range cases {
		if got := isCancelledStopReason(reason); got != want {
			t.Errorf("isCancelledStopReason(%q) = %v, want %v", reason, got, want)
		}
	}
}

func TestSessionUpdateKind(t *testing.T) {
	cases := map[string]string{
		`{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk"}}`: "agent_message_chunk",
		`{"update":{"sessionUpdate":"tool_call"}}`:                           "tool_call",
		`{"update":{}}`:   "",
		`{"no":"update"}`: "",
	}
	for raw, want := range cases {
		if got := sessionUpdateKind(json.RawMessage(raw)); got != want {
			t.Errorf("sessionUpdateKind(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestKnownSilentUpdate(t *testing.T) {
	for _, k := range []string{"agent_thought_chunk", "tool_call", "tool_call_update", "plan"} {
		if !knownSilentUpdate(k) {
			t.Errorf("%q should be a known silent update", k)
		}
	}
	for _, k := range []string{"agent_message_chunk", "some_new_kind", ""} {
		if knownSilentUpdate(k) {
			t.Errorf("%q should NOT be treated as silent", k)
		}
	}
}

func TestCarriesText(t *testing.T) {
	if !carriesText(json.RawMessage(`{"update":{"content":{"type":"text","text":"hi"}}}`)) {
		t.Error("expected carriesText=true for update.content with text")
	}
	if carriesText(json.RawMessage(`{"update":{"sessionUpdate":"plan"}}`)) {
		t.Error("expected carriesText=false when update.content is absent")
	}
	if carriesText(json.RawMessage(`{"update":{"content":{"type":"text","text":"   "}}}`)) {
		t.Error("expected carriesText=false for whitespace-only content")
	}
}
