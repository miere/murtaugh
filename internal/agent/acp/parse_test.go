package acp

import (
	"encoding/json"
	"testing"

	"github.com/miere/murtaugh/internal/agent"
)

func TestExtractTextFromACPAgentMessageChunkUpdate(t *testing.T) {
	raw := json.RawMessage(`{"sessionId":"session-1","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"pong"}}}`)
	if got := extractNotificationText(raw); got != "pong" {
		t.Fatalf("unexpected extracted text: %q", got)
	}
}

func TestExtractTextFromACPAgentMessageUpdate(t *testing.T) {
	raw := json.RawMessage(`{"sessionId":"session-1","update":{"sessionUpdate":"agent_message","content":[{"type":"text","text":"final "},{"type":"text","text":"answer"}]}}`)
	if got := extractNotificationText(raw); got != "final answer" {
		t.Fatalf("unexpected extracted text: %q", got)
	}
}

func TestExtractTextIgnoresACPToolCallContent(t *testing.T) {
	raw := json.RawMessage(`{"sessionId":"session-1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-1","status":"in_progress","content":[{"type":"content","content":{"type":"text","text":"raw tool output"}}]}}`)
	if got := extractNotificationText(raw); got != "" {
		t.Fatalf("expected tool output to be hidden from assistant stream, got %q", got)
	}
}

func TestExtractTaskFromACPNotification(t *testing.T) {
	t.Run("valid task with all fields", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","task":{"id":"task-1","title":"Searching codebase","status":"in_progress","description":"looking for references"}}`)
		task := extractTask(raw)
		if task == nil {
			t.Fatal("expected non-nil task")
		}
		if task.ID != "task-1" {
			t.Fatalf("expected id task-1, got %q", task.ID)
		}
		if task.Title != "Searching codebase" {
			t.Fatalf("expected title 'Searching codebase', got %q", task.Title)
		}
		if task.Status != agent.TaskStatusInProgress {
			t.Fatalf("expected status in_progress, got %q", task.Status)
		}
		if task.Description != "looking for references" {
			t.Fatalf("expected description 'looking for references', got %q", task.Description)
		}
	})

	t.Run("missing id", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","task":{"title":"Foo","status":"pending"}}`)
		if task := extractTask(raw); task != nil {
			t.Fatalf("expected nil for missing id, got %+v", task)
		}
	})

	t.Run("no task field", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","content":{"type":"text","text":"hello"}}`)
		if task := extractTask(raw); task != nil {
			t.Fatalf("expected nil for no task field, got %+v", task)
		}
	})

	t.Run("taskId camelCase alias", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","task":{"taskId":"t-2","title":"Build","status":"complete"}}`)
		task := extractTask(raw)
		if task == nil {
			t.Fatal("expected non-nil task")
		}
		if task.ID != "t-2" {
			t.Fatalf("expected id t-2, got %q", task.ID)
		}
		if task.Status != agent.TaskStatusComplete {
			t.Fatalf("expected status complete, got %q", task.Status)
		}
	})

	t.Run("ACP tool_call update", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"call-1","title":"List files","kind":"read","status":"pending"}}`)
		task := extractTask(raw)
		if task == nil {
			t.Fatal("expected non-nil task")
		}
		if task.ID != "call-1" || task.Title != "List files" || task.Status != agent.TaskStatusPending {
			t.Fatalf("unexpected ACP tool task: %+v", task)
		}
		if task.Description != "read" {
			t.Fatalf("expected kind as description, got %q", task.Description)
		}
	})

	t.Run("ACP tool_call_update completed alias", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-1","status":"completed","content":[{"type":"content","content":{"type":"text","text":"done"}}]}}`)
		task := extractTask(raw)
		if task == nil {
			t.Fatal("expected non-nil task")
		}
		if task.ID != "call-1" || task.Status != agent.TaskStatusComplete || task.Output != "done" {
			t.Fatalf("unexpected ACP tool update: %+v", task)
		}
	})
}

func TestExtractPlanTasks(t *testing.T) {
	t.Run("plan entries become indexed tasks", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"plan","entries":[{"content":"Scan","status":"completed"},{"content":"Fix","status":"in_progress"}]}}`)
		tasks := extractPlanTasks(raw)
		if len(tasks) != 2 {
			t.Fatalf("expected 2 tasks, got %d: %+v", len(tasks), tasks)
		}
		if tasks[0].ID != "plan-0" || tasks[0].Title != "Scan" || tasks[0].Status != agent.TaskStatusComplete {
			t.Fatalf("task 0: %+v", tasks[0])
		}
		if tasks[1].ID != "plan-1" || tasks[1].Title != "Fix" || tasks[1].Status != agent.TaskStatusInProgress {
			t.Fatalf("task 1: %+v", tasks[1])
		}
	})

	t.Run("non-plan update yields nil", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"tool_call","toolCallId":"c1"}}`)
		if tasks := extractPlanTasks(raw); tasks != nil {
			t.Fatalf("expected nil for non-plan update, got %+v", tasks)
		}
	})

	t.Run("entry with no content is skipped but indexes stay stable", func(t *testing.T) {
		raw := json.RawMessage(`{"sessionId":"s1","update":{"sessionUpdate":"plan","entries":[{"content":"","status":"pending"},{"content":"Real","status":"pending"}]}}`)
		tasks := extractPlanTasks(raw)
		if len(tasks) != 1 {
			t.Fatalf("expected the empty entry to be skipped, got %+v", tasks)
		}
		// The kept entry retains its position-based id (the second entry → plan-1).
		if tasks[0].ID != "plan-1" || tasks[0].Title != "Real" {
			t.Fatalf("expected plan-1/Real, got %+v", tasks[0])
		}
	})
}
