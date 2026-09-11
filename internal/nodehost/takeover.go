package nodehost

import (
	"strings"

	"github.com/miere/murtaugh/internal/agent"
)

func (h *Host) markTakeover(sessionID, previousNodeID string) {
	if sessionID == "" {
		return
	}
	h.mu.Lock()
	if h.takeovers == nil {
		h.takeovers = make(map[string]string, 1)
	}
	h.takeovers[sessionID] = previousNodeID
	h.mu.Unlock()
}

func (h *Host) takeTakeover(sessionID string) (previousNodeID string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	previousNodeID, ok = h.takeovers[sessionID]
	if ok {
		delete(h.takeovers, sessionID)
	}
	return previousNodeID, ok
}

const takeoverTag = "conversation-takeover"

func takeoverNotice(previousNodeID string) string {
	var b strings.Builder
	b.WriteString("<")
	b.WriteString(takeoverTag)
	b.WriteString(">\n")
	b.WriteString("This conversation began on a different runtime node")
	if previousNodeID != "" {
		b.WriteString(" (")
		b.WriteString(previousNodeID)
		b.WriteString(")")
	}
	b.WriteString(", which is no longer connected, and has been moved to this one.\n")
	b.WriteString("You are joining part-way through. Nothing the previous node held is available here: ")
	b.WriteString("its agent session, its working directory, and any files or command output it produced are gone. ")
	b.WriteString("Only what appears in the Slack thread above survived the move.\n")
	b.WriteString("If the user refers to earlier work you cannot see, say plainly that the conversation moved machines ")
	b.WriteString("and ask them to re-share what you need. Do not guess at it.\n")
	b.WriteString("</")
	b.WriteString(takeoverTag)
	b.WriteString(">")
	return b.String()
}

func (h *Host) preparePrompt(sessionID string, req agent.PromptRequest) agent.PromptRequest {
	previous, ok := h.takeTakeover(sessionID)
	if !ok {
		return req
	}
	return foldTakeover(req, previous)
}

func foldTakeover(req agent.PromptRequest, previousNodeID string) agent.PromptRequest {
	notice := takeoverNotice(previousNodeID)
	if strings.TrimSpace(req.Text) == "" {
		req.Text = notice
		return req
	}
	req.Text = notice + "\n\n" + req.Text
	return req
}
