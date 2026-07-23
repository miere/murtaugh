package claudecode

import (
	"encoding/json"
	"strings"
)

// control-channel frame accessors. The shapes are Claude Code's internal
// stream-json control protocol, verified against the 2.1.216 binary (spec 019 §6):
//
//	host→CLI request:   {type:"control_request", request_id, request:{subtype, …}}
//	CLI→host response:  {type:"control_response", response:{subtype:"success"|"error", request_id, response|error}}
//	CLI→host request:   {type:"control_request", request_id, request:{subtype:"can_use_tool", …}}

// controlRequestSubtype returns the subtype of an inbound control_request.
func controlRequestSubtype(m *streamMessage) string {
	return rawField(m.Request, "subtype")
}

// controlResponseSubtype returns "success"/"error" for a control_response.
func controlResponseSubtype(m *streamMessage) string {
	return rawField(m.Response, "subtype")
}

// controlResponseRequestID returns the request_id a control_response echoes (it
// is nested inside the `response` object, not at the top level).
func controlResponseRequestID(m *streamMessage) string {
	return rawField(m.Response, "request_id")
}

// controlResponseError returns the error string of a failed control_response.
func controlResponseError(m *streamMessage) string {
	return rawField(m.Response, "error")
}

// parseCanUseTool pulls the tool name and (raw) input out of a can_use_tool
// request, tolerating the field-name variants the protocol has used.
func parseCanUseTool(request json.RawMessage) (toolName string, input json.RawMessage) {
	var p struct {
		ToolName  string          `json:"tool_name"`
		ToolName2 string          `json:"toolName"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(request, &p); err != nil {
		return "", nil
	}
	toolName = firstNonEmpty(p.ToolName, p.ToolName2, p.Name)
	return toolName, p.Input
}

// rawField decodes obj (a JSON object) and returns key as a string, coercing a
// number to its decimal form. Returns "" when absent or obj is not an object.
func rawField(obj json.RawMessage, key string) string {
	if len(obj) == 0 {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(obj, &m); err != nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	return rawString(v)
}

// rawString renders a JSON scalar as a string: an unquoted string, a number
// verbatim, or the trimmed raw for anything else.
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return strings.Trim(string(raw), `"`)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
