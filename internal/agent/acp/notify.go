package acp

import (
	"encoding/json"

	"github.com/miere/murtaugh/internal/agent"
)

func (c *ProcessClient) deliverNotification(notification rpcNotification) {
	if notification.Method != "session/update" {
		// Surface any ACP notification we don't implement so a protocol feature we
		// silently ignore is visible in the log rather than invisible (the class of
		// gap that hid the dropped permission request).
		c.log.Warn("ignoring unhandled ACP notification", "method", notification.Method)
		return
	}
	var params map[string]any
	if err := json.Unmarshal(notification.Params, &params); err != nil {
		return
	}
	sessionID, _ := params["sessionId"].(string)
	if sessionID == "" {
		sessionID, _ = params["session_id"].(string)
	}
	if sessionID == "" {
		return
	}
	c.mu.Lock()
	sub := c.subscribers[sessionID]
	scope := c.dests[sessionID]
	if sub != nil {
		// Register as an in-flight sender while still holding the lock that guards
		// the map, so teardown either sees us here (and waits) or has already
		// retracted the subscription (and we bail below). Balanced by Done once all
		// the sends in this call have landed.
		sub.wg.Add(1)
	}
	c.mu.Unlock()
	if sub == nil {
		return
	}
	defer sub.wg.Done()
	ch := sub.events
	kind := sessionUpdateKind(notification.Params)
	c.log.Debug("ACP session/update", "session_id", sessionID, "update", kind)

	if task := extractTask(notification.Params); task != nil {
		// Feed the turn's tool watcher so its heartbeat knows a tool is in flight
		// (and for how long). tool_call updates are the only signal a long,
		// output-silent tool gives; plan updates below are the agent's task list,
		// not tool execution, so they are deliberately not watched.
		c.mu.Lock()
		w := c.toolWatch[sessionID]
		c.mu.Unlock()
		if w != nil {
			w.observe(task.ID, task.Title, task.Status)
		}
		// Block on the send: dropping task or text notifications truncates the
		// agent response in the consumer (chat handler). The readLoop is back-
		// pressured by the consumer, which is the intended behaviour.
		ch <- agent.Event{Type: agent.EventTask, Task: task}
		return
	}
	// An ACP `plan` update is the agent's structured task list. Render each entry
	// as a task event — the same structured surface native gets from its per-tool
	// task events — so the plan shows as task cards / a tool block instead of being
	// dropped and leaking into the reply prose as plain markdown (which the stream
	// then paginates, splitting the message). Entries carry no id, so each is keyed
	// by its position: a stable id across the plan's snapshot updates.
	if tasks := extractPlanTasks(notification.Params); len(tasks) > 0 {
		for _, task := range tasks {
			ch <- agent.Event{Type: agent.EventTask, Task: task}
		}
		return
	}
	// A single agent message can carry binary content blocks (an image, audio, or
	// an embedded resource blob) alongside its text. Surface each as a first-class
	// attachment the chat handler uploads — emitted ahead of the text so the file
	// lands before the prose that introduces it. Block on the send for the same
	// back-pressure reason as text/task above.
	for _, a := range extractAttachments(notification.Params) {
		ch <- agent.Event{Type: agent.EventAttachment, Attachment: a}
	}
	event := agent.Event{Type: agent.EventText, Text: extractNotificationText(notification.Params)}
	if event.Text == "" {
		// An update we neither rendered as a task nor recognised as agent text.
		// Thought chunks etc. are expected and silent; but if an *unrecognised*
		// kind carries text we'd otherwise drop it, which looks like an empty
		// reply — log it at WARN so protocol drift (e.g. a goose update changing
		// the answer's envelope) is visible rather than silent.
		if kind != "" && !knownSilentUpdate(kind) && carriesText(notification.Params) {
			c.log.Warn("ACP session/update carried text under an unhandled kind; reply may appear empty", "session_id", sessionID, "update", kind)
		}
		return
	}
	// Record that this turn streamed reply text, so the prompt goroutine does not
	// re-emit the final result payload (the same text) as one trailing block.
	if scope.sawText != nil {
		scope.sawText.Store(true)
	}
	ch <- event
}
