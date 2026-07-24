package acp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/miere/murtaugh/internal/agent"
)

func extractTask(raw json.RawMessage) *agent.TaskEvent {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return nil
	}
	m, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if taskMap, ok := m["task"].(map[string]any); ok {
		return taskFromMap(taskMap)
	}
	updateMap, ok := m["update"].(map[string]any)
	if !ok {
		return nil
	}
	sessionUpdate, _ := updateMap["sessionUpdate"].(string)
	if sessionUpdate != "tool_call" && sessionUpdate != "tool_call_update" {
		return nil
	}
	return taskFromMap(updateMap)
}

// extractPlanTasks turns an ACP `plan` session/update into one task event per
// plan entry, or nil when the update is not a plan. A plan is a full snapshot of
// the agent's task list on each update, and its entries carry no id, so each entry
// is keyed by its index ("plan-0", "plan-1", …) — stable across the snapshots so
// the consumer updates the same cards in place rather than stacking duplicates.
// An entry with no content is skipped (a blank card carries nothing).
func extractPlanTasks(raw json.RawMessage) []*agent.TaskEvent {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return nil
	}
	m, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	updateMap, ok := m["update"].(map[string]any)
	if !ok {
		return nil
	}
	if su, _ := updateMap["sessionUpdate"].(string); su != "plan" {
		return nil
	}
	entries, ok := updateMap["entries"].([]any)
	if !ok {
		return nil
	}
	var tasks []*agent.TaskEvent
	for i, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		title := planEntryTitle(entry)
		if title == "" {
			continue
		}
		task := &agent.TaskEvent{ID: fmt.Sprintf("plan-%d", i), Title: title, Kind: agent.TaskKindPlan}
		if status, ok := entry["status"].(string); ok {
			task.Status = normalizeTaskStatus(status)
		}
		tasks = append(tasks, task)
	}
	return tasks
}

// planEntryTitle pulls the human-readable label off a plan entry, tolerating the
// few shapes agents use: a "content" string (ACP's field), or a "title"/"text"
// fallback.
func planEntryTitle(entry map[string]any) string {
	for _, key := range []string{"content", "title", "text"} {
		if s, ok := entry[key].(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	return ""
}

func taskFromMap(taskMap map[string]any) *agent.TaskEvent {
	id, _ := taskMap["id"].(string)
	if id == "" {
		id, _ = taskMap["taskId"].(string)
	}
	if id == "" {
		id, _ = taskMap["toolCallId"].(string)
	}
	if id == "" {
		id, _ = taskMap["tool_call_id"].(string)
	}
	if id == "" {
		return nil
	}
	task := &agent.TaskEvent{ID: id}
	if title, ok := taskMap["title"].(string); ok {
		task.Title = title
	}
	if desc, ok := taskMap["description"].(string); ok {
		task.Description = desc
	}
	if task.Description == "" {
		if kind, ok := taskMap["kind"].(string); ok {
			task.Description = kind
		}
	}
	if status, ok := taskMap["status"].(string); ok {
		task.Status = normalizeTaskStatus(status)
	}
	if content, ok := taskMap["content"]; ok {
		task.Output = strings.Join(extractStrings(content), "")
	}
	return task
}

func normalizeTaskStatus(status string) agent.TaskStatus {
	switch agent.TaskStatus(status) {
	case agent.TaskStatusComplete, "completed":
		return agent.TaskStatusComplete
	case agent.TaskStatusFailed:
		return agent.TaskStatusFailed
	case agent.TaskStatusCancelled:
		return agent.TaskStatusCancelled
	case agent.TaskStatusPending:
		return agent.TaskStatusPending
	case agent.TaskStatusInProgress:
		return agent.TaskStatusInProgress
	default:
		return agent.TaskStatus(status)
	}
}
