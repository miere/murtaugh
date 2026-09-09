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

// This file is HEADLESS DISPATCH (#199): running a scheduled job, a workflow
// trigger or a link unfurl on a node when there is no user to fleet on.
//
// # Why this cannot go through delegation
//
// A chat turn has an initiator, and item 10 chooses a node out of that person's
// fleet. A cron at 03:00 has nobody. Neither has an unfurl in any useful sense:
// the sharer is whichever workspace member pasted a link, usually somebody who
// owns no node and holds no grant — so fleeting on them would mean every unfurl
// by a non-node-owner failed, which is not a policy, it is an outage with a
// user id attached.
//
// So Host.delegate is left exactly as it is, including fleetFor's early return
// for an empty user id, which delegate.go says in as many words is written to
// resist the edit that would relax it. Headless work gets its own selection
// path, and the path selects ONE node: the main node.
//
// # The main node is designated by the gateway
//
// access.main_node, keyed by node id, written by the gateway admin. Item 4
// settled that a node must never assert its own identity, and being main is the
// largest grant this gateway makes — the right to serve every user's unfurls
// and every scheduled job — so it is the one claim a node is least entitled to
// make about itself. Nothing on agentwire.Advertisement changes here.
//
// # Nothing is borrowed, and nothing fails quietly
//
// There is no fallback to "whichever node is attached". A gateway with no main
// node designated, or whose main node is asleep, refuses — by name, in the log,
// and in the journal, because #199 exists to prevent the failure where jobs
// stop firing and nobody is told. Borrowing a node instead would run a stranger's
// 03:00 job on a laptop whose owner never agreed to it, and would do so silently.

// ErrNoMainNode is what a headless surface gets when this gateway has no main
// node designated at all. It is a configuration gap, not a transient one, and
// waiting will not fix it.
var ErrNoMainNode = errors.New("no main node is designated on this gateway (set access.main_node to a node id), so headless work — scheduled jobs, workflow triggers, link unfurling — cannot run")

// ErrMainNodeOffline is what they get when one IS designated and is not
// attached. Distinct from ErrNoMainNode because the two need different people:
// this one is "go and wake the machine", the other is "go and configure the
// gateway", and one message covering both sends half the readers to the wrong
// place.
var ErrMainNodeOffline = errors.New("the main node is not attached to this gateway, so headless work cannot run; a job whose node is asleep does not run and is not replayed")

// headlessDelegator is agentruntime.Delegator over the main node.
//
// It is the gateway's answer to all four of the delegate-to-agent consumers
// that have no user: scheduled jobs, the workflow engine's reply-to-slack arm,
// link unfurling, and the `jobs.run` tool. The fifth — the CLI's own runner —
// is untouched and stays in process, which #170 is explicit about: when the
// broker is broken there must be a way to run an agent that does not go
// through it.
type headlessDelegator struct {
	host *Host
	// idleTimeout bounds a delegation by inactivity, taken from the same
	// runtime default the in-process runner uses, so a job does not behave
	// differently for having crossed a link.
	idleTimeout time.Duration
	log         *slog.Logger
}

// RunForJSON drives a delegation on the main node and requires JSON back. It
// backs the workflow engine's reply-to-slack arm and link unfurling, both of
// which render the result.
func (d *headlessDelegator) RunForJSON(ctx context.Context, agentName, prompt string) ([]byte, error) {
	out, err := d.run(ctx, agentName, prompt, "run_for_json")
	if err != nil {
		return nil, err
	}
	return oneshot.ExpectJSON(out, agentName, d.log)
}

// RunAndForget drives a delegation on the main node and discards the text. It
// backs scheduled jobs and top-level workflow triggers, where the agent is
// expected to act through its own tools rather than hand anything back.
func (d *headlessDelegator) RunAndForget(ctx context.Context, agentName, prompt string) error {
	out, err := d.run(ctx, agentName, prompt, "run_and_forget")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		d.log.Debug("delegate-to-agent discarding fire-and-forget output", "agent", agentName, "bytes", len(out))
	}
	return nil
}

// run resolves the main node and drives one turn on it.
func (d *headlessDelegator) run(ctx context.Context, agentName, prompt, verb string) (string, error) {
	node, err := d.host.mainNode(ctx, agentName, verb)
	if err != nil {
		return "", err
	}
	d.log.Info("delegating headless work to the main node", "node_id", node.nodeID, "agent", agentName, "verb", verb)
	// The node's CLIENT is deliberately neither initialized nor closed here.
	// Both happened, and will happen, at the connection's own boundaries: the
	// handshake initialized it, and closing it would drop the node rather than
	// the delegation. That asymmetry with the in-process runner is why
	// internal/oneshot takes a client instead of making one.
	//
	// Its SESSION is a different lifetime and oneshot.Drive ends it, which over
	// a link is the difference between a delegation and a leak: nothing else
	// would, because an ephemeral session belongs to no conversation and the
	// node's session manager never sees it. Left open it is one retained session
	// — and on ACP one live subprocess — per job, per workflow trigger and per
	// pasted link, for as long as the node stays up.
	return oneshot.Drive(ctx, node.client, oneshot.Request{
		Agent:       agentName,
		Prompt:      prompt,
		IdleTimeout: d.idleTimeout,
	})
}

// mainNode resolves the designated main node, or says loudly why it could not.
//
// Both refusals are journalled at ERROR under a kind of their own, because the
// log alone is not enough: the failure #199 exists to prevent is a scheduled job
// that stops running with nobody told, and a line in slack.err.log at 03:00 is
// how that failure looked the last time. The journal is what the troubleshoot
// bundle carries and what `journal.query` searches.
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
	// Advisory, not a gate. Nothing on the wire can address a profile by name
	// yet — agentwire.InitializeResult says so, and the chat path routes to a
	// node without naming one either — so refusing here would be stricter than
	// delegation and would break a working single-profile node whose profile is
	// called something else. But a job named against an agent the main node does
	// not serve is a real misconfiguration that otherwise produces a plausible
	// answer from the wrong profile, so it is said out loud.
	if profiles := h.claimOf(node).Profiles; len(profiles) > 0 && agentName != "" && !slices.Contains(profiles, agentName) {
		h.log.Warn("headless work names an agent the main node does not advertise; it will run on whichever profile that node serves",
			"agent", agentName, "node_id", node.nodeID, "advertised", profiles)
	}
	return node, nil
}

// headlessRefusal records a dispatch that could not happen.
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
