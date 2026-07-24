package acp

import (
	"fmt"
	"strings"

	"github.com/miere/murtaugh/internal/agent"
)

// promptBlocks renders a agent.PromptRequest into ACP `session/prompt` content
// blocks. ACP exposes no system role, so leading delimited blocks are the
// closest stand-in for a system note. Order:
//  0. a <persona> block (only when a shared persona is configured) carrying
//     Murtaugh's voice, so an ACP agent reads as the same character as native
//     even when it runs in an external project with its own AGENTS.md.
//  1. a <context> block carrying the volatile per-turn facts (current time,
//     working directory) — the ACP analogue of native's RenderTurnContext, so
//     an ACP agent knows what day it is and where it is rooted, just like the
//     native loop. Emitted for every caller, chat or CLI.
//  2. a <conversation-context> block (only when the prompt carries a Slack
//     conversation) telling the agent where it is talking so it can hand the
//     same channel/thread to the `restart` tool. Kept as a separate block with
//     machine-readable channel/thread attributes so that parseability is
//     unchanged.
//  3. the thread transcript, when History is set (a freshly opened session
//     backfilling an existing thread).
//  4. the user's text.
func (c *ProcessClient) promptBlocks(request agent.PromptRequest) []map[string]string {
	blocks := make([]map[string]string, 0, 5)
	if persona := strings.TrimSpace(c.opts.Persona); persona != "" {
		blocks = append(blocks, map[string]string{"type": "text", "text": "<persona>\n" + persona + "\n</persona>"})
	}
	if ctxText := c.renderTurnContext(); ctxText != "" {
		blocks = append(blocks, map[string]string{"type": "text", "text": ctxText})
	}
	if request.Channel != "" {
		ctxText := fmt.Sprintf(
			"<conversation-context channel=%q thread=%q>You are responding in this Slack conversation. "+
				"If you call the `restart` tool, pass these exact channel and thread values so the approval "+
				"card is asked here.</conversation-context>",
			request.Channel, request.Thread,
		)
		blocks = append(blocks, map[string]string{"type": "text", "text": ctxText})
	}
	if request.History != "" {
		blocks = append(blocks, map[string]string{"type": "text", "text": request.History})
	}
	blocks = append(blocks, map[string]string{"type": "text", "text": request.Text})
	return blocks
}

// renderTurnContext renders the volatile per-turn <context> block (current time
// and working directory) for an ACP prompt, or "" when there is nothing to say.
// It mirrors the native RenderTurnContext format so the two backends present the
// same facts to the model; the Slack location is intentionally left to the
// separate <conversation-context> block above.
func (c *ProcessClient) renderTurnContext() string {
	var lines []string
	if c.now != nil {
		if now := c.now(); !now.IsZero() {
			lines = append(lines, "It is currently "+now.Format("2006-01-02 15:04 MST"))
		}
	}
	if cwd := c.sessionCWD(); cwd != "" && cwd != "." {
		lines = append(lines, "Working directory: "+cwd)
	}
	if len(lines) == 0 {
		return ""
	}
	return "<context>\n" + strings.Join(lines, "\n") + "\n</context>"
}
