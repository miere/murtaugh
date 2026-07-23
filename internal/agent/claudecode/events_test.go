package claudecode

import (
	"testing"

	"github.com/miere/murtaugh/internal/agent"
)

func TestDecodeAndClassify(t *testing.T) {
	cases := []struct {
		line      string
		isControl bool
		isResult  bool
	}{
		{`{"type":"assistant","message":{"role":"assistant","content":[]}}`, false, false},
		{`{"type":"control_request","request_id":"x","request":{"subtype":"can_use_tool"}}`, true, false},
		{`{"type":"control_response","response":{"subtype":"success","request_id":"x"}}`, true, false},
		{`{"type":"result","subtype":"success","stop_reason":"end_turn"}`, false, true},
	}
	for _, tc := range cases {
		m, err := decodeMessage([]byte(tc.line))
		if err != nil {
			t.Fatalf("decode %q: %v", tc.line, err)
		}
		if m.isControl() != tc.isControl {
			t.Errorf("isControl(%q)=%v want %v", tc.line, m.isControl(), tc.isControl)
		}
		if m.isResult() != tc.isResult {
			t.Errorf("isResult(%q)=%v want %v", tc.line, m.isResult(), tc.isResult)
		}
	}
}

func TestAssistantEventsTextAndToolUse(t *testing.T) {
	line := `{"type":"assistant","parent_tool_use_id":null,"message":{"role":"assistant","content":[
		{"type":"thinking","thinking":"hmm"},
		{"type":"text","text":"working"},
		{"type":"tool_use","id":"toolu_1","name":"Glob","input":{"pattern":"*.txt"}}
	]}}`
	m, _ := decodeMessage([]byte(line))
	evs := m.toEvents()
	if len(evs) != 2 {
		t.Fatalf("expected 2 events (text + tool_use), got %d: %+v", len(evs), evs)
	}
	if evs[0].Type != agent.EventText || evs[0].Text != "working" {
		t.Errorf("event[0] = %+v", evs[0])
	}
	if evs[1].Type != agent.EventTask || evs[1].Task.ID != "toolu_1" || evs[1].Task.Title != "Glob" ||
		evs[1].Task.Status != agent.TaskStatusInProgress {
		t.Errorf("event[1] = %+v", evs[1].Task)
	}
}

func TestToolResultRetiresCall(t *testing.T) {
	ok := `{"type":"user","parent_tool_use_id":"toolu_1","message":{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"toolu_1","content":"3 files"}]}}`
	m, _ := decodeMessage([]byte(ok))
	evs := m.toEvents()
	if len(evs) != 1 || evs[0].Task.Status != agent.TaskStatusComplete || evs[0].Task.Output != "3 files" {
		t.Fatalf("expected complete tool result, got %+v", evs)
	}

	bad := `{"type":"user","message":{"role":"user","content":[
		{"type":"tool_result","tool_use_id":"toolu_2","is_error":true,"content":[{"type":"text","text":"boom"}]}]}}`
	m2, _ := decodeMessage([]byte(bad))
	evs2 := m2.toEvents()
	if len(evs2) != 1 || evs2[0].Task.Status != agent.TaskStatusFailed || evs2[0].Task.Output != "boom" {
		t.Fatalf("expected failed tool result with array content, got %+v", evs2)
	}
}

func TestSubagentLifecycleEvents(t *testing.T) {
	started := `{"type":"system","subtype":"task_started","task_id":"t1","description":"Count files","subagent_type":"general-purpose"}`
	m, _ := decodeMessage([]byte(started))
	evs := m.toEvents()
	if len(evs) != 1 || evs[0].Task.ID != "t1" || evs[0].Task.Status != agent.TaskStatusInProgress ||
		evs[0].Task.Description != "general-purpose" {
		t.Fatalf("task_started mapping = %+v", evs)
	}

	updated := `{"type":"system","subtype":"task_updated","task_id":"t1","patch":{"status":"completed"}}`
	m2, _ := decodeMessage([]byte(updated))
	evs2 := m2.toEvents()
	if len(evs2) != 1 || evs2[0].Task.Status != agent.TaskStatusComplete {
		t.Fatalf("task_updated(completed) mapping = %+v", evs2)
	}

	// A non-terminal patch produces nothing.
	running := `{"type":"system","subtype":"task_updated","task_id":"t1","patch":{"status":"running"}}`
	m3, _ := decodeMessage([]byte(running))
	if evs3 := m3.toEvents(); len(evs3) != 0 {
		t.Fatalf("non-terminal task_updated should yield no events, got %+v", evs3)
	}

	notif := `{"type":"system","subtype":"task_notification","task_id":"t1","status":"completed","output_file":"/x"}`
	m4, _ := decodeMessage([]byte(notif))
	evs4 := m4.toEvents()
	if len(evs4) != 1 || evs4[0].Task.Status != agent.TaskStatusComplete {
		t.Fatalf("task_notification mapping = %+v", evs4)
	}
}

func TestExtractText(t *testing.T) {
	if got := extractText([]byte(`"plain"`)); got != "plain" {
		t.Errorf("string content=%q", got)
	}
	if got := extractText([]byte(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)); got != "ab" {
		t.Errorf("array content=%q", got)
	}
	if got := extractText(nil); got != "" {
		t.Errorf("empty content=%q", got)
	}
}

func TestIgnoredEnvelopes(t *testing.T) {
	for _, line := range []string{
		`{"type":"system","subtype":"init","session_id":"s"}`,
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":5}`,
		`{"type":"rate_limit_event"}`,
	} {
		m, _ := decodeMessage([]byte(line))
		if evs := m.toEvents(); len(evs) != 0 {
			t.Errorf("expected no events for %q, got %+v", line, evs)
		}
	}
}

func TestControlFrameAccessors(t *testing.T) {
	req, _ := decodeMessage([]byte(`{"type":"control_request","request_id":"abc","request":{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"o"}}}`))
	if got := controlRequestSubtype(req); got != "can_use_tool" {
		t.Errorf("controlRequestSubtype=%q", got)
	}
	if got := rawString(req.RequestID); got != "abc" {
		t.Errorf("request_id=%q", got)
	}
	name, input := parseCanUseTool(req.Request)
	if name != "Write" {
		t.Errorf("tool name=%q", name)
	}
	if len(input) == 0 || string(input) == "null" {
		t.Errorf("expected tool input, got %s", input)
	}

	resp, _ := decodeMessage([]byte(`{"type":"control_response","response":{"subtype":"error","request_id":"abc","error":"nope"}}`))
	if controlResponseSubtype(resp) != "error" || controlResponseRequestID(resp) != "abc" || controlResponseError(resp) != "nope" {
		t.Errorf("control_response accessors wrong: sub=%q id=%q err=%q",
			controlResponseSubtype(resp), controlResponseRequestID(resp), controlResponseError(resp))
	}
}
