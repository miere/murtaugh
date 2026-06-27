package migrate

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// detectV1 reports whether dir still carries the pre-v1 (legacy) shape: the old
// slack.yaml anchor, an `acp:`/`agent:` runtime block in agents.yaml, or a
// `configuration:` block in gateway.yaml.
func detectV1(dir string) bool {
	if fileExists(filepath.Join(dir, "slack.yaml")) {
		return true
	}
	if m := readYAML(filepath.Join(dir, "agents.yaml")); m != nil {
		if _, ok := m["acp"]; ok {
			return true
		}
		if _, ok := m["agent"]; ok {
			return true
		}
	}
	if m := readYAML(filepath.Join(dir, "gateway.yaml")); m != nil {
		if _, ok := m["configuration"]; ok {
			return true
		}
	}
	return false
}

// applyV1 converts a legacy config directory to the v1 shape:
//   - slack.yaml → gateway.yaml, with `configuration:`→`access:`, the global and
//     per-channel no-mention lists folded under `chat.no_mention`, `commands:`
//     dropped, and `workflow-rules`/`unfurl-rules` extracted to sibling files.
//   - agents.yaml `acp:`/`agent:` runtime block → grouped `defaults:`; its
//     `enabled` moves to `chat.enabled`; `stream_final_feedback` is dropped.
//   - each agent's flat backend fields → a `native:` or `acp:` sub-block, and
//     `acp_permission` → `approval.requests`.
func applyV1(dir string) error {
	// The anchor may already be named gateway.yaml (a partial prior run); prefer
	// slack.yaml when present.
	rootPath := filepath.Join(dir, "slack.yaml")
	if !fileExists(rootPath) {
		rootPath = filepath.Join(dir, "gateway.yaml")
	}
	root := readYAML(rootPath)
	if root == nil {
		root = map[string]any{}
	}

	// configuration → access; lift the global no-mention list out for the fold.
	var noMentionEverywhere any
	if cfg, ok := asMap(root["configuration"]); ok {
		if v, ok := cfg["do_not_require_mention_from"]; ok {
			noMentionEverywhere = v
			delete(cfg, "do_not_require_mention_from")
		}
		root["access"] = cfg
		delete(root, "configuration")
	}

	chat, _ := asMap(root["chat"])
	if chat == nil {
		chat = map[string]any{}
	}
	// Fold both no-mention lists under chat.no_mention.
	noMention := map[string]any{}
	if noMentionEverywhere != nil {
		noMention["everywhere"] = noMentionEverywhere
	}
	if v, ok := chat["channel_do_not_require_mention"]; ok {
		noMention["by_channel"] = v
		delete(chat, "channel_do_not_require_mention")
	}
	if len(noMention) > 0 {
		chat["no_mention"] = noMention
	}

	// The inert commands block is removed (slash verbs are hardcoded).
	delete(root, "commands")

	// Extract the rule blocks into their own files.
	if wr, ok := root["workflow-rules"]; ok {
		if err := writeYAML(filepath.Join(dir, "workflow-rules.yaml"), map[string]any{"workflow-rules": wr}); err != nil {
			return err
		}
		delete(root, "workflow-rules")
	}
	if ur, ok := root["unfurl-rules"]; ok {
		if err := writeYAML(filepath.Join(dir, "unfurl-rules.yaml"), map[string]any{"unfurl-rules": ur}); err != nil {
			return err
		}
		delete(root, "unfurl-rules")
	}

	// --- agents.yaml ---
	agentsPath := filepath.Join(dir, "agents.yaml")
	agentsDoc := readYAML(agentsPath)
	if agentsDoc == nil {
		agentsDoc = map[string]any{}
	}
	// `agent:` is the newer spelling of the old runtime block; it wins over `acp:`.
	runtime, _ := asMap(agentsDoc["acp"])
	if r, ok := asMap(agentsDoc["agent"]); ok {
		runtime = r
	}
	if runtime != nil {
		// enabled gated the whole agent runtime; it is now the chat-surface gate.
		// Preserve current behaviour 1:1 (false stays false, true stays true).
		if v, ok := runtime["enabled"]; ok {
			chat["enabled"] = v
		}
		agentsDoc["defaults"] = buildDefaults(runtime)
		delete(agentsDoc, "acp")
		delete(agentsDoc, "agent")
	}

	if agents, ok := asMap(agentsDoc["agents"]); ok {
		for name, raw := range agents {
			if p, ok := asMap(raw); ok {
				agents[name] = migrateProfile(p)
			}
		}
	}

	root["chat"] = chat

	// Write the new files, then drop the legacy anchor.
	if err := writeYAML(filepath.Join(dir, "gateway.yaml"), root); err != nil {
		return err
	}
	if err := writeYAML(agentsPath, agentsDoc); err != nil {
		return err
	}
	if slack := filepath.Join(dir, "slack.yaml"); fileExists(slack) {
		if err := os.Remove(slack); err != nil {
			return fmt.Errorf("remove legacy slack.yaml: %w", err)
		}
	}
	return nil
}

// buildDefaults maps the old flat runtime block to the grouped defaults block.
// stream_final_feedback and enabled are intentionally dropped.
func buildDefaults(r map[string]any) map[string]any {
	session := map[string]any{}
	moveKey(session, r, "session_idle_timeout", "idle_timeout")
	moveKey(session, r, "request_timeout", "request_timeout")
	moveKey(session, r, "max_sessions", "max_concurrent")

	rendering := map[string]any{}
	moveKey(rendering, r, "progress_display", "progress_display")
	moveKey(rendering, r, "stream_min_chunk_chars", "stream_min_chunk_chars")
	moveKey(rendering, r, "stream_append_interval", "stream_append_interval")

	acp := map[string]any{}
	moveKey(acp, r, "startup_timeout", "startup_timeout")
	moveKey(acp, r, "cancel_grace_period", "cancel_grace_period")

	defaults := map[string]any{}
	if len(session) > 0 {
		defaults["session"] = session
	}
	if len(rendering) > 0 {
		defaults["rendering"] = rendering
	}
	if len(acp) > 0 {
		defaults["acp"] = acp
	}
	return defaults
}

// nativeFields are the backend-specific keys that move under a `native:` block.
var nativeFields = []string{
	"provider", "model", "base_url", "api_key_env",
	"system_prompt", "system_prompt_file", "max_turns",
	"context_limit", "compaction", "cache_retention",
}

// acpFields are the backend-specific keys that move under an `acp:` block.
var acpFields = []string{"command", "args", "interruptible", "env"}

// sharedProfileFields stay at the top level of the agent profile.
var sharedProfileFields = []string{"workdir", "tools", "mcp_servers", "export_skills_to_fs", "progress_display"}

// migrateProfile rewrites one agent: shared knobs stay on top, backend fields
// move under native:/acp: (chosen by the old kind / presence of command), and
// acp_permission folds into approval.requests.
func migrateProfile(p map[string]any) map[string]any {
	kind, _ := p["kind"].(string)
	_, hasCommand := p["command"]
	isACP := kind == "acp" || (kind == "" && hasCommand)

	out := map[string]any{}
	for _, k := range sharedProfileFields {
		moveKey(out, p, k, k)
	}

	approval, _ := asMap(p["approval"])
	if approval == nil {
		approval = map[string]any{}
	}
	if v, ok := p["acp_permission"]; ok {
		approval["requests"] = v
	}
	if len(approval) > 0 {
		out["approval"] = approval
	}

	if isACP {
		acp := map[string]any{}
		for _, k := range acpFields {
			moveKey(acp, p, k, k)
		}
		out["acp"] = acp
	} else {
		native := map[string]any{}
		for _, k := range nativeFields {
			moveKey(native, p, k, k)
		}
		out["native"] = native
	}
	return out
}

// --- small helpers -------------------------------------------------------

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readYAML(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil
	}
	return m
}

func writeYAML(path string, v any) error {
	data, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// asMap coerces a decoded YAML value to map[string]any. yaml.v3 decodes mappings
// as map[string]interface{}, so this is a plain type assertion.
func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

// moveKey moves src[from] to dst[to] when present, deleting it from src.
func moveKey(dst, src map[string]any, from, to string) {
	if v, ok := src[from]; ok {
		dst[to] = v
		delete(src, from)
	}
}
