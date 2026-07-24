package agent

import "github.com/google/uuid"

// sessionNamespace is the fixed UUIDv5 namespace for deriving a session id from a
// Slack conversation. It is a FROZEN CONTRACT: never change it, and never change
// the canonicalKey format in DeriveSessionID — for a backend that persists
// sessions by this id (claude_code's --session-id/--resume), a change orphans
// every on-disk session. The only sanctioned reset is bumping the "v1" prefix.
var sessionNamespace = uuid.MustParse("6d757274-6175-6768-4363-763100000001")

// DeriveSessionID returns a deterministic, restart-invariant id for a Slack
// conversation — a pure function of its identity, so the same conversation maps
// to the same id every time with nothing persisted. It is the single home for
// this derivation, shared by every backend that needs a stable per-conversation
// id: a caller-specified session id it can resume across restarts (claude_code),
// or just a collision-free routing key for one process per conversation (acp).
//
// The team + channel + thread triple identifies the conversation; a DM's channel
// id (a `D…` id) already distinguishes it, so no separate dm flag is needed.
func DeriveSessionID(meta SessionMetadata) string {
	key := "v1|team=" + meta.TeamID + "|channel=" + meta.ChannelID + "|thread=" + meta.ThreadTS
	return uuid.NewSHA1(sessionNamespace, []byte(key)).String()
}
