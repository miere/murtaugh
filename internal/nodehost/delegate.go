package nodehost

import (
	"context"
	"errors"
	"fmt"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/journal"
)

// Separate from ErrNoNode: nothing connected is the admin's problem, while
// none of the user's own nodes connected is the user's.
var ErrNoFleet = errors.New("no runtime node of yours is connected, and you hold no grant on another")

type delegation struct {
	node     *attached
	takeover bool
	previous string
}

func (h *Host) delegate(ctx context.Context, meta agent.SessionMetadata) (delegation, error) {
	nodes := h.connected()
	if len(nodes) == 0 {
		return delegation{}, ErrNoNode
	}

	key, keyed := agent.ConversationFromContext(ctx)
	ref := config.ConversationRef{TeamID: key.TeamID, ChannelID: key.ChannelID, ThreadTS: key.ThreadTS, DM: key.DM}
	pinnable := keyed && ref.Valid() && h.pins != nil

	var previous string
	if pinnable {
		pin, found, err := h.pins.Get(ctx, ref)
		if err != nil {
			return delegation{}, fmt.Errorf("read this conversation's node pin: %w", err)
		}
		if found {
			if node := pickByID(nodes, pin.NodeID); node != nil {
				h.log.Debug("conversation is pinned to a connected node", "node_id", node.nodeID,
					"channel", meta.ChannelID, "thread", meta.ThreadTS)
				return delegation{node: node}, nil
			}
			previous = pin.NodeID
		}
	}

	fleet := h.fleetFor(nodes, meta.UserID)
	if len(fleet) == 0 {
		return delegation{}, ErrNoFleet
	}
	elected := h.elect(fleet, meta.ChannelID, meta.ChannelName)

	if pinnable {
		pin := config.ConversationPin{
			Conversation: ref,
			NodeID:       elected.nodeID,
			UserID:       meta.UserID,
			ElectedAt:    h.now().UTC(),
		}
		if err := h.pins.Put(ctx, pin); err != nil {
			h.log.Error("could not store this conversation's node pin; a later turn may be re-elected elsewhere",
				"error", err, "node_id", elected.nodeID, "channel", meta.ChannelID)
		}
	}

	if previous != "" {
		h.log.Info("re-elected a conversation whose node is gone", "previous_node_id", previous,
			"node_id", elected.nodeID, "channel", meta.ChannelID, "thread", meta.ThreadTS)
		h.rec.Record(ctx, journal.Event{
			Stream:  journal.StreamGateway,
			Kind:    "delegation",
			Level:   journal.LevelWarn,
			Summary: "A conversation moved to another runtime node",
			Keys: journal.Keys{
				TeamID: meta.TeamID, ChannelID: meta.ChannelID,
				ThreadTS: meta.ThreadTS, UserID: meta.UserID,
			},
			Payload: map[string]any{
				"state":            "takeover",
				"previous_node_id": previous,
				"node_id":          elected.nodeID,
				"fleet":            len(fleet),
			},
		})
		return delegation{node: elected, takeover: true, previous: previous}, nil
	}
	h.log.Info("delegated a conversation to a runtime node", "node_id", elected.nodeID,
		"user_id", meta.UserID, "channel", meta.ChannelID, "thread", meta.ThreadTS, "fleet", len(fleet))
	return delegation{node: elected}, nil
}

func (h *Host) fleetFor(nodes []*attached, userID string) []*attached {
	if userID == "" {
		return nil
	}
	own := make([]*attached, 0, len(nodes))
	for _, node := range nodes {
		if node.userID == userID {
			own = append(own, node)
		}
	}
	if len(own) > 0 {
		return own
	}
	access := h.accessConfig()
	granted := make([]*attached, 0, len(nodes))
	for _, node := range nodes {
		if access.GrantsOn(node.nodeID, userID) {
			granted = append(granted, node)
		}
	}
	return granted
}

func (h *Host) elect(fleet []*attached, channelID, channelName string) *attached {
	matching := make([]*attached, 0, len(fleet))
	for _, node := range fleet {
		if _, ok := h.claimOf(node).ClaimFor(channelID, channelName); ok {
			matching = append(matching, node)
		}
	}
	switch len(matching) {
	case 1:
		return matching[0]
	case 0:
		return h.roundRobin(fleet)
	default:
		return h.roundRobin(matching)
	}
}

func (h *Host) roundRobin(candidates []*attached) *attached {
	next := h.cursor.Add(1) - 1
	return candidates[next%uint64(len(candidates))]
}

func pickByID(nodes []*attached, nodeID string) *attached {
	if nodeID == "" {
		return nil
	}
	for _, node := range nodes {
		if node.nodeID == nodeID {
			return node
		}
	}
	return nil
}

func (h *Host) connected() []*attached {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.collapsedLocked()
}

func (h *Host) claimOf(node *attached) agentwire.Advertisement {
	h.mu.Lock()
	defer h.mu.Unlock()
	return node.ad
}
