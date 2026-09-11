package agentwire

import "github.com/miere/murtaugh/internal/agent"

type SessionMetadata struct {
	TeamID    string `json:"team_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	// ChannelName is only for the node's logs; delegation is decided on the gateway's copy.
	ChannelName string `json:"channel_name,omitempty"`
	ThreadTS    string `json:"thread_ts,omitempty"`
	UserID      string `json:"user_id,omitempty"`
	Source      string `json:"source,omitempty"`
	Surface     string `json:"surface,omitempty"`
	CanvasID    string `json:"canvas_id,omitempty"`
	// Ephemeral must survive the hop: without it every delegation hashes to the same session id
	// and claude_code resumes the previous delegation's transcript.
	Ephemeral bool `json:"ephemeral,omitempty"`
	// Headless must survive the hop, or a node raises approvals for a 03:00 job that no thread
	// can answer, and the job blocks until its timeout.
	Headless bool `json:"headless,omitempty"`
}

func EncodeSessionMetadata(m agent.SessionMetadata) SessionMetadata {
	return SessionMetadata{
		TeamID:      m.TeamID,
		ChannelID:   m.ChannelID,
		ChannelName: m.ChannelName,
		ThreadTS:    m.ThreadTS,
		UserID:      m.UserID,
		Source:      m.Source,
		Surface:     m.Surface,
		CanvasID:    m.CanvasID,
		Ephemeral:   m.Ephemeral,
		Headless:    m.Headless,
	}
}

func (m SessionMetadata) Decode() agent.SessionMetadata {
	return agent.SessionMetadata{
		TeamID:      m.TeamID,
		ChannelID:   m.ChannelID,
		ChannelName: m.ChannelName,
		ThreadTS:    m.ThreadTS,
		UserID:      m.UserID,
		Source:      m.Source,
		Surface:     m.Surface,
		CanvasID:    m.CanvasID,
		Ephemeral:   m.Ephemeral,
		Headless:    m.Headless,
	}
}

// History is carried inline despite its size: the node cannot get it any other way, and a
// side transfer would stop a prompt fitting in one frame.
type PromptRequest struct {
	Text    string `json:"text"`
	Channel string `json:"channel,omitempty"`
	Thread  string `json:"thread,omitempty"`
	User    string `json:"user,omitempty"`
	History string `json:"history,omitempty"`
}

func EncodePromptRequest(r agent.PromptRequest) PromptRequest {
	return PromptRequest{
		Text:    r.Text,
		Channel: r.Channel,
		Thread:  r.Thread,
		User:    r.User,
		History: r.History,
	}
}

func (r PromptRequest) Decode() agent.PromptRequest {
	return agent.PromptRequest{
		Text:    r.Text,
		Channel: r.Channel,
		Thread:  r.Thread,
		User:    r.User,
		History: r.History,
	}
}

// Empty is a type rather than a nil body so a method's shape can grow a field without
// becoming a different frame.
type Empty struct{}

type InitializeResult struct {
	// Interruptible is a pointer because a plain bool would read a silent node as "cannot be
	// interrupted"; absent means unknown, which degrades to interruptible.
	Interruptible *bool `json:"interruptible,omitempty"`
	// Advertisement rides the handshake answer so the claim is in hand when the gateway builds
	// its registry entry; a zero value is an unconfigured node, not an absence.
	Advertisement Advertisement `json:"advertisement,omitzero"`
}

type NewSessionResult struct {
	SessionID string `json:"session_id"`
}

// PromptBody's answer is an acceptance, not the turn's outcome: both consumers read Prompt's
// error before rendering, so a rejection must come back as a response.
type PromptBody struct {
	SessionID string        `json:"session_id"`
	Prompt    PromptRequest `json:"prompt"`
}

// Cancel answers success for an unknown session because the gateway's idle path cancels
// under a five-second deadline, where an error is noise.
type SessionRef struct {
	SessionID string `json:"session_id"`
}
