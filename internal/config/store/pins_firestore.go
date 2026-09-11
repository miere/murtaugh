package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"cloud.google.com/go/firestore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/miere/murtaugh/internal/config"
)

type firestoreConversationPins struct {
	client *firestore.Client
	root   string
}

const (
	fsPinTeamID    = "team_id"
	fsPinChannelID = "channel_id"
	fsPinThreadTS  = "thread_ts"
	fsPinDM        = "dm"
	fsPinNodeID    = "node_id"
	fsPinUserID    = "user_id"
	fsPinElectedAt = "elected_at"
)

func openFirestoreConversationPins(ctx context.Context, fsc config.FirestoreConfig) (config.ConversationPinStore, error) {
	client, err := newFirestoreClient(ctx, fsc)
	if err != nil {
		return nil, err
	}
	return &firestoreConversationPins{client: client, root: fsc.EffectiveCollection()}, nil
}

func (s *firestoreConversationPins) Close() error { return s.client.Close() }

func (s *firestoreConversationPins) pins() *firestore.CollectionRef {
	return s.client.Collection(s.root + "_conversation_pins")
}

func pinDocID(ref config.ConversationRef) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		ref.TeamID, ref.ChannelID, ref.ThreadTS, fmt.Sprint(dmFlag(ref.DM)),
	}, "\x00")))
	return itemDocID("pin", hex.EncodeToString(sum[:16]))
}

func (s *firestoreConversationPins) Get(ctx context.Context, ref config.ConversationRef) (config.ConversationPin, bool, error) {
	snap, err := s.pins().Doc(pinDocID(ref)).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return config.ConversationPin{}, false, nil
	}
	if err != nil {
		return config.ConversationPin{}, false, fmt.Errorf("look up conversation pin: %w", err)
	}
	nodeID, err := docString(snap, fsPinNodeID)
	if err != nil {
		return config.ConversationPin{}, false, fmt.Errorf("read conversation pin: %w", err)
	}
	userID, err := docString(snap, fsPinUserID)
	if err != nil {
		return config.ConversationPin{}, false, fmt.Errorf("read conversation pin: %w", err)
	}
	raw, err := docString(snap, fsPinElectedAt)
	if err != nil {
		return config.ConversationPin{}, false, fmt.Errorf("read conversation pin: %w", err)
	}
	at, err := parseStamp(raw)
	if err != nil {
		return config.ConversationPin{}, false, fmt.Errorf("read conversation pin: %w", err)
	}
	return config.ConversationPin{Conversation: ref, NodeID: nodeID, UserID: userID, ElectedAt: at}, true, nil
}

func (s *firestoreConversationPins) Put(ctx context.Context, pin config.ConversationPin) error {
	if err := pin.Validate(); err != nil {
		return err
	}
	ref := pin.Conversation
	if _, err := s.pins().Doc(pinDocID(ref)).Set(ctx, map[string]any{
		fsPinTeamID:    ref.TeamID,
		fsPinChannelID: ref.ChannelID,
		fsPinThreadTS:  ref.ThreadTS,
		fsPinDM:        dmFlag(ref.DM),
		fsPinNodeID:    pin.NodeID,
		fsPinUserID:    pin.UserID,
		fsPinElectedAt: stampKey(pin.ElectedAt),
	}); err != nil {
		return fmt.Errorf("store conversation pin for %q: %w", ref.ChannelID, err)
	}
	return nil
}
