// Package claudecode drives Claude Code directly over its stream-json protocol —
// the transport the Claude Agent SDK wraps — as a third agent.Client, alongside
// the ACP ProcessClient and the in-process native loop. Unlike the ACP path it
// speaks Claude Code's own NDJSON envelope (system/assistant/user/result plus a
// bidirectional control channel for the initialize handshake, tool permissions,
// interrupt, and targeted background-task stop). See Obsidian spec 019.
//
// This file is the pure codec: it decodes one stdout NDJSON line into a
// streamMessage and maps the user-visible ones onto agent.Event. Control
// messages (control_request/control_response) are handled by the client, not
// here — toEvents deliberately returns nothing for them.
package claudecode

import (
	"encoding/json"
	"strings"

	"github.com/miere/murtaugh/internal/agent"
)

// streamMessage is one line of Claude Code stream-json. Only the fields Murtaugh
// consumes are decoded; the envelope carries many more we ignore.
type streamMessage struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype"`
	Message json.RawMessage `json:"message"`
	// ParentToolUseID attributes a nested (subagent) message to the parent tool
	// call that spawned it; empty/null at the top level.
	ParentToolUseID string `json:"parent_tool_use_id"`
	SessionID       string `json:"session_id"`

	// result fields
	StopReason string `json:"stop_reason"`
	Result     string `json:"result"`

	// control fields (see client.go for handling)
	RequestID json.RawMessage `json:"request_id"`
	Request   json.RawMessage `json:"request"`
	Response  json.RawMessage `json:"response"`

	// system/task_* lifecycle fields
	TaskID       string     `json:"task_id"`
	Status       string     `json:"status"`
	Description  string     `json:"description"`
	OutputFile   string     `json:"output_file"`
	LastToolName string     `json:"last_tool_name"`
	SubagentType string     `json:"subagent_type"`
	Patch        *taskPatch `json:"patch"`
}

type taskPatch struct {
	Status string `json:"status"`
}

// innerMessage is the `message` payload of an assistant/user stream message: a
// role plus an ordered list of content blocks.
type innerMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"` // text | thinking | tool_use | tool_result
	Text string `json:"text"`

	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// decodeMessage parses one NDJSON line. A blank line yields (nil, nil).
func decodeMessage(line []byte) (*streamMessage, error) {
	if len(strings.TrimSpace(string(line))) == 0 {
		return nil, nil
	}
	var m streamMessage
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// isControl reports whether the message is a control-channel frame the client
// must handle out-of-band rather than surface as an event.
func (m *streamMessage) isControl() bool {
	return m.Type == "control_request" || m.Type == "control_response" || m.Type == "control_cancel_request"
}

// isResult reports whether this message ends the current turn.
func (m *streamMessage) isResult() bool { return m.Type == "result" }

// toEvents maps a (non-control, non-result) message to zero or more agent.Events
// in emission order. Unknown/ignored messages (thinking_tokens, rate_limit_event,
// system/init) yield nil. A `result` is handled by the client (it emits
// EventComplete with StopReason and closes the turn), so it is not mapped here.
func (m *streamMessage) toEvents() []agent.Event {
	switch m.Type {
	case "assistant":
		return m.assistantEvents()
	case "user":
		return m.toolResultEvents()
	case "system":
		return m.systemEvents()
	default:
		// rate_limit_event and any other envelope we don't render.
		return nil
	}
}

// assistantEvents turns an assistant message's content blocks into text and
// tool-call events. Thinking blocks are not streamed into the reply.
func (m *streamMessage) assistantEvents() []agent.Event {
	inner := m.inner()
	if inner == nil {
		return nil
	}
	var out []agent.Event
	for _, b := range inner.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out = append(out, agent.Event{Type: agent.EventText, Text: b.Text})
			}
		case "tool_use":
			out = append(out, agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{
				ID:     b.ID,
				Title:  b.Name,
				Status: agent.TaskStatusInProgress,
			}})
		}
	}
	return out
}

// toolResultEvents retires the matching tool call when a user message carries
// tool_result blocks (the model feeding a tool's output back to itself).
func (m *streamMessage) toolResultEvents() []agent.Event {
	inner := m.inner()
	if inner == nil {
		return nil
	}
	var out []agent.Event
	for _, b := range inner.Content {
		if b.Type != "tool_result" {
			continue
		}
		status := agent.TaskStatusComplete
		if b.IsError {
			status = agent.TaskStatusFailed
		}
		out = append(out, agent.Event{Type: agent.EventTask, Task: &agent.TaskEvent{
			ID:     b.ToolUseID,
			Status: status,
			Output: extractText(b.Content),
		}})
	}
	return out
}

// systemEvents maps the system/task_* subagent lifecycle. background completion
// (task_notification) is surfaced as a status event for now; routing it back into
// the originating Slack thread is Phase 2 (spec 019 §5).
func (m *streamMessage) systemEvents() []agent.Event {
	switch m.Subtype {
	case "task_started":
		title := m.Description
		return []agent.Event{{Type: agent.EventTask, Task: &agent.TaskEvent{
			ID:          m.TaskID,
			Title:       title,
			Status:      agent.TaskStatusInProgress,
			Description: m.SubagentType,
		}}}
	case "task_progress":
		return []agent.Event{{Type: agent.EventTask, Task: &agent.TaskEvent{
			ID:     m.TaskID,
			Title:  m.Description,
			Status: agent.TaskStatusInProgress,
		}}}
	case "task_updated":
		if m.Patch != nil && isTerminalStatus(m.Patch.Status) {
			return []agent.Event{{Type: agent.EventTask, Task: &agent.TaskEvent{
				ID:     m.TaskID,
				Status: normalizeStatus(m.Patch.Status),
			}}}
		}
		return nil
	case "task_notification":
		// The in-band background-completion signal. Phase 1 marks the card
		// complete; Phase 2 reads OutputFile and re-enters the thread.
		return []agent.Event{{Type: agent.EventTask, Task: &agent.TaskEvent{
			ID:     m.TaskID,
			Status: normalizeStatus(m.Status),
		}}}
	default:
		// system/init, thinking_tokens, background_tasks_changed: nothing to render.
		return nil
	}
}

func (m *streamMessage) inner() *innerMessage {
	if len(m.Message) == 0 {
		return nil
	}
	var inner innerMessage
	if err := json.Unmarshal(m.Message, &inner); err != nil {
		return nil
	}
	return &inner
}

func isTerminalStatus(s string) bool {
	switch s {
	case "completed", "complete", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func normalizeStatus(s string) agent.TaskStatus {
	switch s {
	case "completed", "complete":
		return agent.TaskStatusComplete
	case "failed":
		return agent.TaskStatusFailed
	case "cancelled":
		return agent.TaskStatusCancelled
	case "in_progress", "running":
		return agent.TaskStatusInProgress
	case "pending":
		return agent.TaskStatusPending
	default:
		return agent.TaskStatus(s)
	}
}

// extractText flattens a tool_result `content` (a string, or an array of
// {type:text,text:...} blocks) into a single string.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, blk := range blocks {
			b.WriteString(blk.Text)
		}
		return b.String()
	}
	return ""
}
