package config

import (
	"context"
	"errors"
	"strings"
	"time"
)

// No user ID on purpose: a channel's session is shared, so its pin is too, and later speakers
// ride the node the first speaker's fleet elected.
type ConversationRef struct {
	TeamID    string
	ChannelID string
	ThreadTS  string
	// DM keeps a direct-message thread apart from a channel thread that carries the same id.
	DM bool
}

// A ref with no channel would share one row with every other channel-less conversation, so
// callers skip pinning it and re-elect every turn instead.
func (r ConversationRef) Valid() bool { return strings.TrimSpace(r.ChannelID) != "" }

type ConversationPin struct {
	Conversation ConversationRef
	// NodeID comes from the credential the gateway resolved, never from a node's own claim.
	NodeID string
	// UserID is for operators asking why a channel is on a machine; the election never reads it back.
	UserID    string
	ElectedAt time.Time
}

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

// Put overwrites, unlike NodeTokenStore: a pin left on a node that is gone would re-elect every
// turn and land somewhere new each time, which is what pinning exists to prevent.
type ConversationPinStore interface {
	Get(ctx context.Context, ref ConversationRef) (ConversationPin, bool, error)

	Put(ctx context.Context, pin ConversationPin) error

	Close() error
}
