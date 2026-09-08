package config

import (
	"context"
	"errors"
	"strings"
	"time"
)

// This file defines the conversation-pin seam: which runtime node a Slack
// conversation was delegated to, and the store that remembers it (#196, part of
// the split in #170).
//
// It is the FIRST per-conversation state Murtaugh has ever persisted. Sessions
// are a pure in-memory map evicted on an idle timeout, so a pin routinely
// outlives the session it was elected for, and a pin with no live session is
// the steady state rather than an anomaly. That asymmetry is the point: #170
// requires the choice of node to survive a gateway restart and a failover,
// while the agent session deliberately does not.
//
// It sits beside the leader lock, the run claim and the node credential for the
// same reason those do: it is state that gateways serving one Slack app must
// agree on, and the store their shared configuration came from is the only
// place they already agree on. And like those three it is deliberately NOT a
// config section — it is absent from AllSections and AllSingletons, so pins
// never enter the Config every process loads, never appear in `cfg show`, and
// are never carried by Snapshot/Restore.
//
// The last of those has a consequence, and here it is benign in a way it is not
// for node tokens: `cfg db migrate --to <backend>` does not carry pins across,
// and a conversation whose pin is missing is simply re-elected on its next
// turn. A lost credential needs a human to re-enrol a node; a lost pin needs
// nobody. That difference is why this is worth storing at all and not worth
// protecting.

// ConversationRef identifies a Slack conversation for pinning.
//
// It is agent.ConversationKey's four fields, redeclared here so this package
// keeps its stdlib-only surface — the same reason NodeToken does not import
// nodetoken. The two are translated at the one call site that has both.
//
// It carries no user id, on purpose and with a consequence. A channel's
// conversation key is shared by the channel's participants so that a session is
// shared too, which means the pin is keyed by the conversation and not by the
// person whose fleet elected it. In a shared channel the second speaker rides
// the first speaker's node. That is the one place "a fleet is never a mixture"
// and "the session is shared by the channel" meet, and the resolution is that
// the FLEET is only consulted at election: once a conversation is pinned, it is
// pinned for everyone in it.
type ConversationRef struct {
	TeamID    string
	ChannelID string
	ThreadTS  string
	// DM separates a direct-message thread from a channel thread that happens
	// to carry the same id, exactly as the conversation key does.
	DM bool
}

// Valid reports whether the ref names a conversation at all.
//
// A pin with no channel is not a pin: it would collide with every other
// channel-less conversation on one row. Callers treat an invalid ref as "do not
// pin", which degrades delegation to re-electing every turn rather than pinning
// the wrong thing — visible in the round robin, and never wrong.
func (r ConversationRef) Valid() bool { return strings.TrimSpace(r.ChannelID) != "" }

// ConversationPin is one delegated conversation.
type ConversationPin struct {
	Conversation ConversationRef
	// NodeID is the node the conversation is pinned to. It is the node id the
	// gateway resolved from a credential, never one a node asserted.
	NodeID string
	// UserID is whose fleet elected it. It is recorded for the operator's
	// benefit — "why is this channel on that machine" is otherwise unanswerable
	// once the electing turn has scrolled away — and is not read back by the
	// election.
	UserID string
	// ElectedAt is when this node was chosen, UTC. A re-election overwrites it,
	// so the gap between it and now is how long the current node has held the
	// conversation.
	ElectedAt time.Time
}

// Validate reports whether the pin is storable.
func (p ConversationPin) Validate() error {
	if !p.Conversation.Valid() {
		return errors.New("conversation pin: a channel id is required")
	}
	if strings.TrimSpace(p.NodeID) == "" {
		return errors.New("conversation pin: a node id is required")
	}
	if p.ElectedAt.IsZero() {
		return errors.New("conversation pin: elected-at is required")
	}
	return nil
}

// ConversationPinStore holds the delegated-conversation records.
//
// Put OVERWRITES. That is the whole reason this interface exists rather than a
// create-only one like NodeTokenStore's: #170 is explicit that when a pinned
// node becomes unavailable the stored pin must be overwritten and not bypassed,
// because a pin left pointing at a machine that is gone makes the conversation
// re-elect on every single turn — and every turn lands somewhere new, which is
// the "model lost its memory" symptom the pin exists to prevent, in a worse
// form.
// There is deliberately no Delete. Nothing clears a pin: a re-election
// overwrites the row, and a conversation that should move is moved by electing
// again rather than by forgetting. A reset surface would need one — and it can
// arrive with the surface, rather than as three implementations of a method
// nobody calls.
type ConversationPinStore interface {
	// Get returns the pin for a conversation. A false with a nil error means
	// the conversation has never been delegated, which is the ordinary state of
	// every first message.
	Get(ctx context.Context, ref ConversationRef) (ConversationPin, bool, error)

	// Put records the pin, replacing any pin for the same conversation.
	Put(ctx context.Context, pin ConversationPin) error

	// Close releases the backend handle.
	Close() error
}
