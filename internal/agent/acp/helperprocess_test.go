package acp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestACPHelperProcess(t *testing.T) {
	mode := ""
	if len(os.Args) > 0 {
		mode = os.Args[len(os.Args)-1]
	}
	if mode != "acp-helper" && mode != "acp-helper-cancellable" {
		return
	}
	supportsCancel := mode == "acp-helper-cancellable"
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			os.Exit(2)
		}
		switch req.Method {
		case "initialize":
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{
				"protocolVersion": 1,
				"agentCapabilities": map[string]any{
					"loadSession":     true,
					"mcpCapabilities": map[string]any{"http": true, "sse": false},
				},
			}})
		case "session/new":
			var params struct {
				CWD        string `json:"cwd"`
				MCPServers []any  `json:"mcpServers"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil || params.CWD == "" || params.MCPServers == nil {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "invalid session/new params"}})
				continue
			}
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"sessionId": "session-1"}})
		case "session/prompt":
			var params struct {
				Prompt []struct {
					Text string `json:"text"`
				} `json:"prompt"`
			}
			_ = json.Unmarshal(req.Params, &params)
			// The user's text is always the final content block; any leading
			// blocks are the <context>/<conversation-context> stand-ins.
			promptText := ""
			if len(params.Prompt) > 0 {
				promptText = params.Prompt[len(params.Prompt)-1].Text
			}
			if name, ok := strings.CutPrefix(promptText, "echoenv:"); ok {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "agent_message", "content": []map[string]string{{"type": "text", "text": os.Getenv(name)}}}}})
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"stopReason": "end_turn"}})
				continue
			}
			if reply, ok := strings.CutPrefix(promptText, "dupe:"); ok {
				// Stream the reply as a chunk, then echo the SAME text in the prompt
				// result — the shape a streaming agent produces (it both streams and
				// returns the final text). The client must surface it once.
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": reply}}}})
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"stopReason": "end_turn", "content": []map[string]string{{"type": "text", "text": reply}}}})
				continue
			}
			if entries, ok := strings.CutPrefix(promptText, "plan:"); ok {
				// Emit a `plan` update with one entry per comma-separated "title=status"
				// pair, so the client can be checked for turning the plan into task
				// events rather than dropping it.
				var planEntries []map[string]any
				for _, pair := range strings.Split(entries, ",") {
					title, status, _ := strings.Cut(pair, "=")
					planEntries = append(planEntries, map[string]any{"content": title, "status": status})
				}
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "plan", "entries": planEntries}}})
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"stopReason": "end_turn"}})
				continue
			}
			if n := parseBurstCount(promptText); n > 0 {
				for i := 0; i < n; i++ {
					_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": "x"}}}})
				}
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"stopReason": "end_turn"}})
				continue
			}
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "tool_call", "toolCallId": "task-1", "title": "Thinking", "status": "in_progress"}}})
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "task-1", "status": "completed", "content": []map[string]any{{"type": "content", "content": map[string]string{"type": "text", "text": "tool output should not stream"}}}}}})
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": "session-1", "update": map[string]any{"sessionUpdate": "agent_message", "content": []map[string]string{{"type": "text", "text": "Hello from ACP"}}}}})
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"stopReason": "end_turn"}})
		case "session/cancel":
			if supportsCancel {
				// An interruptible agent accepts the call (here: reports the
				// probe's synthetic session is unknown — not method-not-found).
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "unknown session"}})
			} else {
				_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "session/cancel not supported"}})
			}
		default:
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": fmt.Sprintf("unknown method %s", req.Method)}})
		}
	}
	os.Exit(0)
}

func parseBurstCount(prompt string) int {
	const prefix = "burst:"
	if !strings.HasPrefix(prompt, prefix) {
		return 0
	}
	var n int
	if _, err := fmt.Sscanf(prompt[len(prefix):], "%d", &n); err != nil {
		return 0
	}
	return n
}
