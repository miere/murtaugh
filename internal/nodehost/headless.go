package nodehost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/journal"
	"github.com/miere/murtaugh/internal/oneshot"
)

var ErrNoMainNode = errors.New("no main node is designated on this gateway (set access.main_node to a node id), so headless work — scheduled jobs, workflow triggers, link unfurling — cannot run")

// Separate from ErrNoMainNode because it needs someone to wake the machine,
// not someone to configure the gateway.
var ErrMainNodeOffline = errors.New("the main node is not attached to this gateway, so headless work cannot run; a job whose node is asleep does not run and is not replayed")

type headlessDelegator struct {
	host        *Host
	idleTimeout time.Duration
	log         *slog.Logger
}

func (d *headlessDelegator) RunForJSON(ctx context.Context, agentName, prompt string) ([]byte, error) {
	out, _, err := d.run(ctx, agentName, prompt, "run_for_json")
	if err != nil {
		return nil, err
	}
	return oneshot.ExpectJSON(out, agentName, d.log)
}

func (d *headlessDelegator) RunAndForget(ctx context.Context, agentName, prompt string) error {
	out, _, err := d.run(ctx, agentName, prompt, "run_and_forget")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		d.log.Debug("delegate-to-agent discarding fire-and-forget output", "agent", agentName, "bytes", len(out))
	}
	return nil
}

func (d *headlessDelegator) RunForReply(ctx context.Context, agentName, prompt string) (agentruntime.Reply, error) {
	out, node, err := d.run(ctx, agentName, prompt, "run_for_reply")
	if err != nil {
		return agentruntime.Reply{}, err
	}
	return agentruntime.Reply{Text: out, NodeID: node.nodeID, NodeOwner: node.userID}, nil
}

func (d *headlessDelegator) run(ctx context.Context, agentName, prompt, verb string) (string, *attached, error) {
	node, err := d.host.mainNode(ctx, agentName, verb)
	if err != nil {
		return "", nil, err
	}
	d.log.Info("delegating headless work to the main node", "node_id", node.nodeID, "agent", agentName, "verb", verb)
	out, err := oneshot.Drive(ctx, node.client, oneshot.Request{
		Agent:       agentName,
		Prompt:      prompt,
		IdleTimeout: d.idleTimeout,
	})
	return out, node, err
}

func (h *Host) mainNode(ctx context.Context, agentName, verb string) (*attached, error) {
	designated := h.accessConfig().MainNodeID()
	if designated == "" {
		h.headlessRefusal(ctx, "no-main-node", "Headless work could not run: this gateway designates no main node",
			"", agentName, verb, len(h.connected()))
		return nil, ErrNoMainNode
	}
	node := pickByID(h.connected(), designated)
	if node == nil {
		h.headlessRefusal(ctx, "main-node-offline", "Headless work could not run: the main node is not attached",
			designated, agentName, verb, len(h.connected()))
		return nil, fmt.Errorf("%w (node %s)", ErrMainNodeOffline, designated)
	}
	if profiles := h.claimOf(node).Profiles; len(profiles) > 0 && agentName != "" && !slices.Contains(profiles, agentName) {
		h.log.Warn("headless work names an agent the main node does not advertise; it will run on whichever profile that node serves",
			"agent", agentName, "node_id", node.nodeID, "advertised", profiles)
	}
	return node, nil
}

func (h *Host) headlessRefusal(ctx context.Context, state, summary, nodeID, agentName, verb string, connected int) {
	h.log.Error(summary, "state", state, "agent", agentName, "verb", verb,
		"main_node_id", nodeID, "nodes_connected", connected)
	h.rec.Record(ctx, journal.Event{
		Stream:  journal.StreamGateway,
		Kind:    "headless",
		Level:   journal.LevelError,
		Summary: summary,
		Payload: map[string]any{
			"state":           state,
			"agent":           agentName,
			"verb":            verb,
			"main_node_id":    nodeID,
			"nodes_connected": connected,
		},
	})
}

var _ agentruntime.Delegator = (*headlessDelegator)(nil)
