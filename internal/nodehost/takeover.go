package nodehost

import (
	"strings"

	"github.com/miere/murtaugh/internal/agent"
)

// This file is the TAKEOVER notice: what the model is told when a conversation
// it has no memory of arrives on its node because the previous one is gone.
//
// # Why it is folded into the user message
//
// #170 says the block goes into the current user message rather than being sent
// as a message of its own, and the reason is an invariant with a name:
// assertNoConsecutiveUserAfterTool in internal/agent/native/messages.go, run
// before every provider call. A standalone user message appended after a
// tool-result is the Goose MOIM consecutive-user empty-reply bug, and
// native.Conversation deliberately exposes no API for appending per-turn
// context as its own message precisely so nothing can do it by accident.
//
// So the notice is prepended to PromptRequest.Text, which every backend folds
// into the one user message it sends. That choice also answers a second problem
// #170 does not raise but the code does: claude_code renders no context block at
// all (composePrompt folds only History into Text), so a notice carried as a new
// PromptRequest field would be silently dropped by one of the three backends and
// the model would never learn the conversation had moved. Text is the only
// carrier all three honour.
//
// # Why the tag is not <context>
//
// The native and ACP backends already emit a `<context>` block of their own,
// with the time, the working directory and the Slack thread. A second block with
// the same tag in the same message reads as a malformed or duplicated one. The
// distinct tag also makes the notice greppable in a transcript when somebody
// asks why the agent said it could not see earlier work.

// markTakeover records that the next prompt on this session must carry the
// notice, and which node the conversation came from.
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

// takeTakeover consumes the mark, if there is one.
//
// Consumed rather than read, because the notice belongs to the FIRST prompt of
// the new session and to no other. Left in place it would be prepended to every
// turn for the life of the conversation, and a model told on every message that
// it has just arrived and can see nothing behaves as though that were true.
func (h *Host) takeTakeover(sessionID string) (previousNodeID string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	previousNodeID, ok = h.takeovers[sessionID]
	if ok {
		delete(h.takeovers, sessionID)
	}
	return previousNodeID, ok
}

// takeoverTag names the block. Exported nowhere: it exists so the test and the
// renderer cannot drift.
const takeoverTag = "conversation-takeover"

// takeoverNotice is what the model is told.
//
// It says three things, and each earns its line. That the conversation started
// elsewhere, so the thread above is older than this session. That the earlier
// machine's state is gone — not merely the transcript, but the working
// directory and anything written into it — because "you may be missing context"
// invites the model to bluff about files it cannot open. And that it should say
// so, because the alternative is a confident answer about work it cannot see,
// which is worse than the outage.
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

// preparePrompt applies whatever a session's election left owing to the turn
// about to be sent.
//
// Today that is the takeover notice and nothing else. It is a named seam rather
// than two lines inside nodeClient.Prompt so that "what the gateway adds to a
// prompt on the way to a node" is one function with one test, instead of a
// habit of prepending things that each looks harmless on its own.
func (h *Host) preparePrompt(sessionID string, req agent.PromptRequest) agent.PromptRequest {
	previous, ok := h.takeTakeover(sessionID)
	if !ok {
		return req
	}
	return foldTakeover(req, previous)
}

// foldTakeover puts the notice inside the prompt's user message.
//
// Text, not History: History is a pre-rendered transcript of the Slack thread
// and a backend is free to emit it as its own content block, which would put the
// notice in the wrong place on exactly the backend that has a separate block to
// put it in. Ahead of the text rather than after it, so the model reads the
// caveat before the request it applies to.
func foldTakeover(req agent.PromptRequest, previousNodeID string) agent.PromptRequest {
	notice := takeoverNotice(previousNodeID)
	if strings.TrimSpace(req.Text) == "" {
		req.Text = notice
		return req
	}
	req.Text = notice + "\n\n" + req.Text
	return req
}
