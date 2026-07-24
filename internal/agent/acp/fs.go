package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// handleAgentRequest serves a request the agent sends to us (the ACP client). The
// only method we implement is session/request_permission; anything else gets a
// method-not-found reply (and a warn) so the agent fails fast instead of blocking
// forever waiting for a response we would otherwise never send.
func (c *ProcessClient) handleAgentRequest(line []byte) {
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		c.log.Warn("ignoring malformed ACP agent request", "error", err)
		return
	}
	switch req.Method {
	case "session/request_permission":
		c.handlePermissionRequest(req.ID, req.Params)
	case "fs/read_text_file":
		c.handleReadTextFile(req.ID, req.Params)
	case "fs/write_text_file":
		c.handleWriteTextFile(req.ID, req.Params)
	default:
		c.log.Warn("unhandled ACP agent request; replying method-not-found", "method", req.Method)
		c.respondError(req.ID, jsonRPCMethodNotFound, "method not implemented by murtaugh ACP client")
	}
}

// handleReadTextFile serves an agent-initiated fs/read_text_file request: the
// agent's Read tool asks us (its client) to read a file on its behalf. We honour
// it only within the agent's workdir so a read can never exfiltrate host files
// outside the project, mirroring how the attach tool is rooted. line (1-based)
// and limit (max lines) narrow the returned slice when present.
func (c *ProcessClient) handleReadTextFile(id, params json.RawMessage) {
	var p struct {
		Path  string `json:"path"`
		Line  *int   `json:"line"`
		Limit *int   `json:"limit"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		c.respondError(id, jsonRPCInvalidParams, fmt.Sprintf("fs/read_text_file: %v", err))
		return
	}
	abs, err := c.resolveWithinWorkDir(p.Path)
	if err != nil {
		c.respondError(id, jsonRPCInvalidParams, err.Error())
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		c.respondError(id, jsonRPCInternalError, fmt.Sprintf("fs/read_text_file: %v", err))
		return
	}
	content := string(data)
	if p.Line != nil || p.Limit != nil {
		content = sliceLines(content, p.Line, p.Limit)
	}
	c.respondResult(id, map[string]any{"content": content})
}

// handleWriteTextFile serves an agent-initiated fs/write_text_file request,
// rooted in the agent's workdir for the same reason as the read. Parent
// directories are created so the agent can write new files under the project.
func (c *ProcessClient) handleWriteTextFile(id, params json.RawMessage) {
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		c.respondError(id, jsonRPCInvalidParams, fmt.Sprintf("fs/write_text_file: %v", err))
		return
	}
	abs, err := c.resolveWithinWorkDir(p.Path)
	if err != nil {
		c.respondError(id, jsonRPCInvalidParams, err.Error())
		return
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		c.respondError(id, jsonRPCInternalError, fmt.Sprintf("fs/write_text_file: %v", err))
		return
	}
	if err := os.WriteFile(abs, []byte(p.Content), 0o644); err != nil {
		c.respondError(id, jsonRPCInternalError, fmt.Sprintf("fs/write_text_file: %v", err))
		return
	}
	c.respondResult(id, nil)
}

// resolveWithinWorkDir cleans a path and verifies it stays inside the agent's
// configured workdir. A relative path is resolved against that root; any path
// that escapes it is rejected so a confused or compromised agent cannot read or
// overwrite host files outside the project it was scoped to.
func (c *ProcessClient) resolveWithinWorkDir(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("fs path is required")
	}
	root := filepath.Clean(c.sessionCWD())
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, abs)
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("fs path %q is outside the agent workdir", p)
	}
	return abs, nil
}

// sliceLines applies ACP's optional line/limit window: line is a 1-based start,
// limit caps the number of lines. Out-of-range values clamp rather than error.
func sliceLines(content string, line, limit *int) string {
	lines := strings.Split(content, "\n")
	start := 0
	if line != nil && *line > 1 {
		start = *line - 1
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := len(lines)
	if limit != nil && *limit >= 0 && start+*limit < end {
		end = start + *limit
	}
	return strings.Join(lines[start:end], "\n")
}
