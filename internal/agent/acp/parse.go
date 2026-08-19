package acp

import (
	"encoding/base64"
	"encoding/json"
	"mime"
	"path"
	"strings"

	"github.com/miere/murtaugh/internal/agent"
)

func extractText(raw json.RawMessage) string {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.Join(extractStrings(value), "")
}

func extractNotificationText(raw json.RawMessage) string {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	m, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	updateMap, ok := m["update"].(map[string]any)
	if !ok {
		return strings.Join(extractStrings(value), "")
	}
	if sessionUpdate, _ := updateMap["sessionUpdate"].(string); sessionUpdate != "agent_message_chunk" && sessionUpdate != "agent_message" {
		return ""
	}
	if content, ok := updateMap["content"]; ok {
		return strings.Join(extractStrings(content), "")
	}
	return strings.Join(extractStrings(updateMap), "")
}

// extractAttachments pulls binary content blocks (image, audio, or an embedded
// resource blob) out of an agent_message_chunk/agent_message session/update and
// decodes them into AttachmentEvents the chat handler can upload. It is
// deliberately limited to agent messages — content on user-message or tool-call
// updates is not the agent replying with a file — and to embedded bytes: a
// resource_link block carries only a URI (no bytes) and is left to the text path
// to mention. Anything malformed (bad base64, missing data) is skipped rather
// than failing the turn.
func extractAttachments(raw json.RawMessage) []*agent.AttachmentEvent {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return nil
	}
	m, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	update, ok := m["update"].(map[string]any)
	if !ok {
		return nil
	}
	if su, _ := update["sessionUpdate"].(string); su != "agent_message_chunk" && su != "agent_message" {
		return nil
	}
	content, ok := update["content"]
	if !ok {
		return nil
	}
	var out []*agent.AttachmentEvent
	for _, block := range contentBlocks(content) {
		if a := attachmentFromBlock(block); a != nil {
			out = append(out, a)
		}
	}
	return out
}

// contentBlocks normalises an ACP content field — which may be a single block
// object or an array of them — into a slice of block maps.
func contentBlocks(content any) []map[string]any {
	switch c := content.(type) {
	case []any:
		out := make([]map[string]any, 0, len(c))
		for _, item := range c {
			if bm, ok := item.(map[string]any); ok {
				out = append(out, bm)
			}
		}
		return out
	case map[string]any:
		return []map[string]any{c}
	}
	return nil
}

// attachmentFromBlock decodes one content block into an agent.AttachmentEvent, or nil
// when the block is text, a bare link, or otherwise carries no embedded bytes.
func attachmentFromBlock(block map[string]any) *agent.AttachmentEvent {
	switch t, _ := block["type"].(string); t {
	case "image", "audio":
		data, _ := block["data"].(string)
		mimeType, _ := block["mimeType"].(string)
		return decodeAttachment(data, mimeType, "", t)
	case "resource":
		res, ok := block["resource"].(map[string]any)
		if !ok {
			return nil
		}
		blob, _ := res["blob"].(string)
		if blob == "" {
			return nil // a text resource (or bare link) — nothing to upload
		}
		mimeType, _ := res["mimeType"].(string)
		uri, _ := res["uri"].(string)
		return decodeAttachment(blob, mimeType, uri, "resource")
	default:
		return nil
	}
}

// decodeAttachment base64-decodes an embedded blob and builds an agent.AttachmentEvent
// with a best-effort filename derived from the resource URI or the mimetype.
// Returns nil when the payload is empty or not valid base64.
func decodeAttachment(b64, mimeType, uri, kind string) *agent.AttachmentEvent {
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(data) == 0 {
		return nil
	}
	return &agent.AttachmentEvent{
		Filename: attachmentFilename(uri, mimeType, kind),
		Mimetype: mimeType,
		Data:     data,
	}
}

// attachmentFilename derives a download name: the URI's base name when present,
// otherwise "<kind><ext>" with the extension inferred from the mimetype.
func attachmentFilename(uri, mimeType, kind string) string {
	if uri != "" {
		if base := path.Base(uri); base != "." && base != "/" && base != "" {
			return base
		}
	}
	name := kind
	if name == "" {
		name = "attachment"
	}
	if exts, err := mime.ExtensionsByType(mimeType); err == nil && len(exts) > 0 {
		return name + exts[0]
	}
	return name
}

// extractStopReason pulls the agent's stop reason out of a session/prompt
// result, tolerating either the camelCase (ACP spec) or snake_case spelling.
// Returns "" when none is present.
func extractStopReason(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, key := range []string{"stopReason", "stop_reason"} {
		if s, ok := m[key].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// isCancelledStopReason reports whether a prompt result ended because the turn
// was cancelled. ACP spells it "cancelled"; the American spelling is accepted
// too, since the value crosses a process boundary from an adapter we do not
// control and getting it wrong reads as a normal completion.
func isCancelledStopReason(stopReason string) bool {
	switch strings.ToLower(strings.TrimSpace(stopReason)) {
	case "cancelled", "canceled":
		return true
	}
	return false
}

// sessionUpdateKind returns the update.sessionUpdate discriminator of a
// session/update notification, or "" when absent. Used for diagnostics.
func sessionUpdateKind(raw json.RawMessage) string {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	m, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	update, ok := m["update"].(map[string]any)
	if !ok {
		return ""
	}
	kind, _ := update["sessionUpdate"].(string)
	return kind
}

// knownSilentUpdate reports whether an update kind is one we deliberately do
// not turn into a chat reply (reasoning, plans, tool bookkeeping). These are
// expected to carry no agent-message text, so dropping them is not drift.
func knownSilentUpdate(kind string) bool {
	switch kind {
	case "agent_thought_chunk", "tool_call", "tool_call_update",
		"plan", "available_commands_update", "current_mode_update", "user_message_chunk":
		return true
	default:
		return false
	}
}

// carriesText reports whether a session/update's update.content holds any
// non-empty text. Used to detect an unrecognised kind that is smuggling the
// agent's reply (protocol drift) so we can log rather than silently drop it.
func carriesText(raw json.RawMessage) bool {
	var value any
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return false
	}
	m, ok := value.(map[string]any)
	if !ok {
		return false
	}
	update, ok := m["update"].(map[string]any)
	if !ok {
		return false
	}
	content, ok := update["content"]
	if !ok {
		return false
	}
	return strings.TrimSpace(strings.Join(extractStrings(content), "")) != ""
}

func extractStrings(value any) []string {
	switch v := value.(type) {
	case map[string]any:
		if text, ok := v["text"].(string); ok {
			return []string{text}
		}
		var out []string
		for _, key := range []string{"update", "content", "delta", "message", "chunks", "updates"} {
			if child, ok := v[key]; ok {
				out = append(out, extractStrings(child)...)
			}
		}
		return out
	case []any:
		var out []string
		for _, child := range v {
			out = append(out, extractStrings(child)...)
		}
		return out
	default:
		return nil
	}
}
