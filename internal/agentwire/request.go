package agentwire

import "github.com/miere/murtaugh/internal/agent"

// This file is the REQUEST direction: gateway → node. The event stream runs the
// other way and is the rest of the package.
//
// The vocabulary is agent.Client's, because agent.Client is the seam the split
// happens at. Putting the remote client at agent.Client — under the existing
// SessionManager rather than in place of it — is what keeps four of the six
// capability assertions in the gateway satisfied by *agent.SessionManager
// exactly as they are today; only the two that are made against the CLIENT
// (CloseSession and SupportsCancel) have to be answered on the wire, and both
// are answered here.

// SessionMetadata is the wire form of agent.SessionMetadata.
//
// Declared afresh with snake_case tags rather than reused, for the reason the
// whole package is: the internal type must stay free to change. agent's own
// camelCase tags are not a contract with anything — nothing marshals that type
// anywhere — so there is nothing to preserve by copying them, and matching the
// house convention (see internal/journal, and Event above) matters more.
type SessionMetadata struct {
	TeamID    string `json:"team_id,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	// ChannelName travels because the gateway is the only side that can resolve
	// it and the node's own logs are otherwise a wall of channel ids. It plays
	// no part in delegation, which is decided before this frame is built and on
	// the gateway's copy of the value.
	ChannelName string `json:"channel_name,omitempty"`
	ThreadTS    string `json:"thread_ts,omitempty"`
	UserID      string `json:"user_id,omitempty"`
	Source      string `json:"source,omitempty"`
	Surface     string `json:"surface,omitempty"`
	CanvasID    string `json:"canvas_id,omitempty"`
	// Ephemeral must survive the hop. A node that loses it derives the session
	// id from an empty conversation triple, every delegation hashes to the same
	// id, and a claude_code backend resumes the previous delegation's
	// transcript — the bug fixed in e2c3cca, re-introduced by a dropped bool.
	Ephemeral bool `json:"ephemeral,omitempty"`
	// Headless must survive the hop for the same class of reason as Ephemeral,
	// and the damage is worse. It is the node's only way to know that no human
	// is behind this turn: a node that loses it raises a real approval frame for
	// a 03:00 job, the gateway has no thread to answer it in, and the job blocks
	// until its own timeout burns. See agent.SessionMetadata.Headless.
	Headless bool `json:"headless,omitempty"`
}

// EncodeSessionMetadata renders the metadata for the wire.
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

// Decode rebuilds the metadata the node's backend is given.
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

// PromptRequest is the wire form of agent.PromptRequest.
//
// History is the field that makes this frame large: a freshly opened session
// for an existing Slack thread carries the whole flattened backstory. It is
// carried inline anyway — it is text the node cannot obtain any other way, it
// is bounded by the thread the gateway already read into memory, and giving it
// a side transfer would mean a prompt could not be sent in one frame.
type PromptRequest struct {
	Text    string `json:"text"`
	Channel string `json:"channel,omitempty"`
	Thread  string `json:"thread,omitempty"`
	User    string `json:"user,omitempty"`
	History string `json:"history,omitempty"`
}

// EncodePromptRequest renders the prompt for the wire.
func EncodePromptRequest(r agent.PromptRequest) PromptRequest {
	return PromptRequest{
		Text:    r.Text,
		Channel: r.Channel,
		Thread:  r.Thread,
		User:    r.User,
		History: r.History,
	}
}

// Decode rebuilds the prompt the node's backend is given.
func (r PromptRequest) Decode() agent.PromptRequest {
	return agent.PromptRequest{
		Text:    r.Text,
		Channel: r.Channel,
		Thread:  r.Thread,
		User:    r.User,
		History: r.History,
	}
}

// Empty is the body of a call that takes nothing or returns nothing. It is a
// type rather than a nil body so a method's shape is written down and can grow
// a field without becoming a different frame.
type Empty struct{}

// InitializeResult answers MethodInitialize.
//
// Interruptible folds in what the session manager otherwise obtains by
// type-asserting the client for SupportsCancel. It belongs in this answer
// rather than in a method of its own for three reasons.
//
// First, nothing about it is per-session, and no backend recomputes it: acp
// resolves it once at Initialize (from the agent's advertised methods) and
// caches it; native answers a constant true; claude_code does not implement the
// probe at all, so session_manager.go's cancelCapabilityProber assertion fails
// and resolveInterruptible takes its interruptible-by-default branch. A node
// speaking for any of the three has the answer by the time Initialize returns —
// including "I do not answer this", which is the absent field below.
//
// Second, it is a property of the node's agent binary rather than of a session.
// Third, once agent profiles move to the node, the `interruptible:` config
// override lives where the gateway cannot read it.
//
// It is a POINTER because the degradation must be visible. A plain bool from a
// node that does not set the field decodes as false, which reads as "this agent
// cannot be interrupted" — a lie that silently disables interrupting an
// in-flight turn. Absent means unknown, and unknown degrades to interruptible,
// which is exactly what the session manager does with an unresolved probe.
//
// Advertisement rides here for a different reason, and the reason is ordering.
// It is what the node claims to serve, and the gateway builds its registry entry
// immediately after this answer returns — so a claim carried here is guaranteed
// to be in hand at exactly the moment there is somewhere to put it. A node that
// pushed its opening claim as a MethodAdvertise request instead would be racing
// its own handshake: the frame can reach a gateway whose registry has no entry
// for the connection yet, and the claim is then dropped for a window nobody
// would think to look at. Later changes have no such problem and travel as
// MethodAdvertise.
//
// What it does NOT unlock is addressing a profile. Nothing on the wire names an
// agent — SessionMetadata and PromptBody carry none — so a node listing profile
// names tells the gateway what exists without giving it a way to ask for one.
// That is stated here rather than implied: item 9 records the names, and the
// field on session.new that would make them addressable is a later item's.
type InitializeResult struct {
	Interruptible *bool `json:"interruptible,omitempty"`
	// Advertisement is the node's opening claim. A zero value means the node
	// claims nothing, which is a legitimate answer and not an absence — an
	// unconfigured node is exactly that, and #170 makes it the onboarding
	// trigger rather than an error.
	Advertisement Advertisement `json:"advertisement,omitzero"`
}

// NewSessionResult answers MethodNewSession with the id every later call names.
type NewSessionResult struct {
	SessionID string `json:"session_id"`
}

// PromptBody is MethodPrompt's request.
//
// Its answer is an acceptance, not the turn's outcome: agent.Client.Prompt
// returns (channel, error) and BOTH consumers read that error synchronously
// before any rendering starts, so a rejected prompt has to come back as a
// response frame. The turn's events then arrive as MessageEvent frames on the
// same request id, ending with End.
type PromptBody struct {
	SessionID string        `json:"session_id"`
	Prompt    PromptRequest `json:"prompt"`
}

// SessionRef is the body of the two calls that name a session and nothing else:
// MethodCancel and MethodCloseSession.
//
// Cancel is idempotent — an unknown session answers success, mirroring
// acp.Client.Cancel returning nil for one — because the gateway's idle path
// cancels under a five-second deadline and an error there is noise, not news.
type SessionRef struct {
	SessionID string `json:"session_id"`
}
