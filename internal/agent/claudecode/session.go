package claudecode

import (
	"github.com/google/uuid"

	"github.com/miere/murtaugh/internal/agent"
)

// murtaughSessionNamespace is the fixed UUIDv5 namespace for deriving Claude Code
// session ids from Slack conversations. It is a FROZEN CONTRACT (spec 019 §2.1):
// never change it — a new namespace orphans every on-disk session at once. The
// only sanctioned reset is bumping the "v1" prefix in canonicalKey below.
var murtaughSessionNamespace = uuid.MustParse("6d757274-6175-6768-4363-763100000001")

// deriveSessionID returns the deterministic Claude Code session id for a Slack
// conversation. It is a pure function of the conversation identity, so the same
// thread re-binds to the same `claude` session across a Murtaugh restart with no
// persisted mapping — the whole point of the stateless design. A new Slack thread
// yields a new ConversationKey → a new id → a fresh session (the natural,
// stateless "start fresh" affordance).
//
// The team + channel + thread triple identifies the conversation; a DM's channel
// id (a `D…` id) already distinguishes it, so no separate dm flag is needed. The
// "v1" prefix is the sole global reset lever: bumping it invalidates every id.
func deriveSessionID(meta agent.SessionMetadata) string {
	key := "v1|team=" + meta.TeamID + "|channel=" + meta.ChannelID + "|thread=" + meta.ThreadTS
	return uuid.NewSHA1(murtaughSessionNamespace, []byte(key)).String()
}
