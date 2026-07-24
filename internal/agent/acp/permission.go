package acp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/miere/murtaugh/internal/agent"
)

// handlePermissionRequest resolves a session/request_permission per the configured
// policy and writes the ACP RequestPermissionResponse. An empty chosen option (no
// human decision, or no allow/reject option to auto-pick) maps to "cancelled".
func (c *ProcessClient) handlePermissionRequest(id, params json.RawMessage) {
	sessionID, title, kind, options := parsePermissionRequest(params)
	optionID := c.decidePermission(sessionID, title, kind, options)
	var outcome map[string]any
	if optionID == "" {
		outcome = map[string]any{"outcome": "cancelled"}
	} else {
		outcome = map[string]any{"outcome": "selected", "optionId": optionID}
	}
	c.respondResult(id, map[string]any{"outcome": outcome})
}

// decidePermission returns the optionId to grant for a permission request, or ""
// to cancel. auto-allow/auto-deny pick a matching option without a human; ask
// raises an agent.EventPermission on the turn's event stream and blocks on the
// consumer's decision, so the request is ordered with the rest of the reply (the
// chat handler settles any open reply text, posts the approval card, and feeds the
// decision back) — mirroring how the native loop gates a tool call inline. ask
// with no live turn (no subscriber) or a cancelled turn denies (returns "") —
// fail-safe and fast, never a hang.
func (c *ProcessClient) decidePermission(sessionID, title, kind string, options []agent.PermissionOption) string {
	// label is for logging only: the title (command/detail) when present, else the
	// kind, else a placeholder.
	label := title
	if label == "" {
		label = kind
	}
	if label == "" {
		label = "a tool"
	}
	switch strings.ToLower(strings.TrimSpace(c.opts.PermissionPolicy)) {
	case "auto-allow":
		return pickOptionByKind(options, "allow")
	case "auto-deny":
		return pickOptionByKind(options, "reject")
	default: // ask
		c.mu.Lock()
		sub := c.subscribers[sessionID]
		scope, ok := c.dests[sessionID]
		if sub != nil {
			sub.wg.Add(1)
		}
		c.mu.Unlock()
		if sub == nil {
			c.log.Warn("ACP permission request with no live turn to ask; denying", "tool", label, "session_id", sessionID)
			return ""
		}
		// Hold the drain barrier only around the send of the permission event, not
		// the human-decision wait below: teardown must be able to close the channel
		// once the ask has landed, without blocking on the operator's click.
		ch := sub.events
		ctx := context.Background()
		if ok && scope.ctx != nil {
			ctx = scope.ctx
		}
		// Buffered so the consumer's reply never blocks even if we have already
		// given up on ctx.Done below.
		decision := make(chan string, 1)
		prompt := &agent.PermissionPrompt{
			Request:  agent.PermissionRequest{SessionID: sessionID, ToolKind: kind, ToolTitle: title, Options: options},
			Decision: decision,
		}
		select {
		case ch <- agent.Event{Type: agent.EventPermission, Permission: prompt}:
			sub.wg.Done()
		case <-ctx.Done():
			sub.wg.Done()
			c.log.Warn("ACP permission request abandoned before it could be asked (turn ended); denying", "tool", label, "session_id", sessionID)
			return ""
		}
		select {
		case optionID := <-decision:
			return optionID
		case <-ctx.Done():
			c.log.Warn("ACP permission request cancelled while awaiting a decision (turn ended); denying", "tool", label, "session_id", sessionID)
			return ""
		}
	}
}

// pickOptionByKind returns the optionId of the first option whose kind matches the
// wanted action ("allow" or "reject"), preferring the _once variant over _always,
// then any kind with the wanted prefix. Returns "" when none match.
func pickOptionByKind(options []agent.PermissionOption, want string) string {
	for _, kind := range []string{want + "_once", want + "_always"} {
		for _, o := range options {
			if o.Kind == kind {
				return o.ID
			}
		}
	}
	for _, o := range options {
		if strings.HasPrefix(o.Kind, want) {
			return o.ID
		}
	}
	return ""
}

// parsePermissionRequest extracts the session id, the tool call's title and kind,
// and the offered options from a session/request_permission params object. Title
// is the agent's human-readable title (for an execute call, the command line);
// kind is the ACP toolCall.kind. They are kept separate so the prompt can show a
// concise label and render the command as its own fenced code block.
func parsePermissionRequest(raw json.RawMessage) (sessionID, title, kind string, options []agent.PermissionOption) {
	var p struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			Title string `json:"title"`
			Kind  string `json:"kind"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	_ = json.Unmarshal(raw, &p)
	sessionID = p.SessionID
	title = p.ToolCall.Title
	kind = p.ToolCall.Kind
	for _, o := range p.Options {
		options = append(options, agent.PermissionOption{ID: o.OptionID, Name: o.Name, Kind: o.Kind})
	}
	return sessionID, title, kind, options
}
