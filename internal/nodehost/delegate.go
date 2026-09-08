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

// This file is DELEGATION (#196): choosing which connected node a Slack
// conversation runs on, remembering the choice, and recovering when the chosen
// node is gone.
//
// # The algorithm is #170's, unaltered
//
//	1. Which nodes in the FLEET match this DM or channel?
//	2. Exactly one — that node takes it.
//	3. More than one — round robin among the matching nodes.
//	4. None — round robin among ALL nodes in the fleet.
//
// Each node's advertisement is its OWN ordered rule list, evaluated first match
// wins, answering yes or no (agentwire.Advertisement.ClaimFor). The gateway
// never merges the lists, which is what makes "no specificity ordering and no
// tie-break" true rather than an omission: there is nothing to order because
// nothing is compared. Two nodes claiming one channel is a misconfiguration
// inside one admin's own fleet, and step 3 answers it without arbitrating.
//
// There is no default node, and none is reachable: step 4 catches every
// unclaimed conversation, so a default would be dead code the day it was
// written.
//
// # The fleet comes first
//
// A conversation belongs to the initiating user's OWN nodes, or to somebody
// else's they hold a GRANT on — never a mixture. Own nodes win outright: a user
// with one node of their own and a grant on three others runs on their own, and
// the grant is what they fall back to when they have brought nothing. That is
// what makes user choice and node claims incapable of conflicting — the
// candidate set is settled before a single claim is read.
//
// # Elect once, then pin
//
// The elected node is stored against the conversation and honoured on every
// later turn. Without that, turn two lands on a node holding no history and the
// model appears to lose its memory mid-conversation, which #170 says gets
// debugged as a model bug for a week.
//
// The pin is consulted BEFORE the fleet, and that ordering is deliberate. A
// pinned conversation is not re-elected, so it is not re-fleeted either: in a
// shared channel the second speaker rides the first speaker's node rather than
// dragging the conversation onto their own. The conversation key omits the user
// on purpose (the session is shared by the channel's participants), and this is
// the one place that meets "a fleet is never a mixture". The resolution is that
// a fleet decides an ELECTION and a pin decides a TURN.
//
// # When the pinned node is gone, the pin is overwritten
//
// Not bypassed. A pin left pointing at a machine that is gone re-elects on every
// single turn, and every turn lands somewhere new — the memory-loss symptom the
// pin exists to prevent, in a worse form, and it survives the node coming back.
// So a re-election writes the new node over the old row before the turn runs.
//
// Round robin needs no persistence at all: because the pin is stored, a cursor
// lost on failover affects only the balance of conversations elected after it.

// ErrNoFleet is what a user gets when nodes are connected but none of them is
// theirs and they hold no grant on any of the others.
//
// It is separate from ErrNoNode because the two need different answers. Nothing
// is connected is the admin's problem; nothing of YOURS is connected is the
// user's, and telling them "no runtime node is connected" while somebody else's
// is happily serving would send them to the wrong person.
var ErrNoFleet = errors.New("no runtime node of yours is connected, and you hold no grant on another")

// delegation is one election's outcome.
type delegation struct {
	// node is the connection the conversation runs on.
	node *attached
	// takeover is true when this conversation was pinned to a node that is no
	// longer connected, so the model is about to see a thread it has no memory
	// of. It is what puts the takeover block on the next prompt.
	takeover bool
	// previous is the node id the conversation was pinned to before, when
	// takeover is true.
	previous string
}

// delegate resolves which node serves a conversation, honouring or replacing
// the stored pin.
//
// It runs on the turn path, from nodeClient.NewSession — which is reached only
// when the session manager has no live session for the conversation, so it is
// once per cold conversation and not once per message. It performs at most one
// read and one write against the pin store.
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
			// Not recoverable by guessing. Electing as though the conversation
			// had never been delegated would move a live conversation onto
			// another machine because a database hiccuped, and the user would
			// see it as the agent forgetting — the failure this whole mechanism
			// exists to prevent. Failing the turn says so instead.
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
			// The turn is servable and the node is chosen, so failing here
			// would deny a working conversation over bookkeeping. What is lost
			// is the guarantee that the NEXT turn lands here too, which is
			// visible as the conversation moving — so it is logged loudly
			// rather than swallowed.
			h.log.Error("could not store this conversation's node pin; a later turn may be re-elected elsewhere",
				"error", err, "node_id", elected.nodeID, "channel", meta.ChannelID)
		}
	}

	if previous != "" {
		h.log.Info("re-elected a conversation whose node is gone", "previous_node_id", previous,
			"node_id", elected.nodeID, "channel", meta.ChannelID, "thread", meta.ThreadTS)
		// Journalled, like a disconnect and for the same reason: this is the
		// event somebody debugging "why did the agent forget" needs to find, and
		// a DM about it would arrive in the conversation that is already about
		// to be told by the model itself.
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

// fleetFor is the candidate set: the user's OWN connected nodes, or — only when
// they have none connected — the connected nodes they hold a grant on.
//
// Never a mixture, and the `if` below is the whole enforcement. It is written as
// an early return rather than as one filtered pass precisely so that a later
// edit cannot accidentally union the two: the granted set is not even built
// when the user has a node of their own.
//
// A caller with no user id at all — a job, an unfurl, a workflow trigger — has
// no fleet, which is #170's item 13 and not this one. It gets an empty fleet
// and ErrNoFleet, which says so, rather than silently borrowing somebody's
// machine.
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

// elect runs #170's four steps over a fleet that is already known non-empty.
//
// The fleet arrives sorted by node id (see Host.connected), which is what makes
// the round robin reproducible: two gateways handed the same fleet and the same
// cursor make the same choice, so a failover does not reshuffle the balance on
// top of everything else it is doing.
func (h *Host) elect(fleet []*attached, channelID, channelName string) *attached {
	matching := make([]*attached, 0, len(fleet))
	for _, node := range fleet {
		if _, ok := h.claimOf(node).ClaimFor(channelID, channelName); ok {
			matching = append(matching, node)
		}
	}
	switch len(matching) {
	case 1:
		// Exactly one node claims it, so there is nothing to balance: it takes
		// it. This is the case the whole assignment mechanism exists for.
		return matching[0]
	case 0:
		// Nobody claimed it. Round robin over the whole fleet — which is why
		// there is no default node to configure and no unrouted conversation.
		return h.roundRobin(fleet)
	default:
		// Two nodes in one admin's own fleet claiming one channel is a
		// misconfiguration, and the gateway deliberately does not arbitrate it.
		return h.roundRobin(matching)
	}
}

// roundRobin advances the cursor and picks.
//
// The cursor lives on the Host rather than in the runtime builder's closure
// because a configuration reload re-runs that builder while the node
// connections survive; a cursor rebuilt on every `cfg` edit would restart the
// rotation at the same node every time and quietly stop balancing on a busy
// gateway.
//
// It is not persisted, and #170 says why: pins are stored, so a cursor lost on
// failover affects only the balance of conversations elected afterwards.
func (h *Host) roundRobin(candidates []*attached) *attached {
	next := h.cursor.Add(1) - 1
	return candidates[next%uint64(len(candidates))]
}

// pickByID finds a connected node by its id.
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

// connected is the registry as delegation sees it: one connection per NODE,
// ordered by node id.
//
// It is Host.collapsedLocked and deliberately not a second implementation of
// it. One per node because a machine holding two live connections through a
// credential rotation must not be round-robinned against itself and called
// balance; ordered because the round robin's determinism is what makes two
// gateways agree, and a Go map's iteration order is deliberately not that.
// Both of those are properties of the REGISTRY, so they live with it — a copy
// here would be a rotation tie-break that could drift from the one the
// registry's own tests exercise.
func (h *Host) connected() []*attached {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.collapsedLocked()
}

// claimOf reads a connection's advertisement under the Host's lock.
//
// An advertisement is the one part of a registry entry that changes while the
// entry lives — a node re-advertises whenever its own configuration changes —
// so it is the one part that cannot be read off the pointer directly.
func (h *Host) claimOf(node *attached) agentwire.Advertisement {
	h.mu.Lock()
	defer h.mu.Unlock()
	return node.ad
}
