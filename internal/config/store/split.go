package store

import (
	"context"
	"fmt"
	"slices"

	"github.com/miere/murtaugh/internal/config"
)

// This file splits ONE combined configuration into the gateway's half and the
// node's half — #170 Change I's migration, and the thing #198 asks to be
// verified from a single combined store.
//
// # It copies, and deletes nothing
//
// The in-process path remains the shipping default through this whole stage, and
// deleting the gateway's agent rows is precisely what would switch it over: a
// gateway whose profiles had been moved out from under it stops being able to
// answer anybody. So the node's half is COPIED, the gateway keeps everything it
// had, and the operator decides when to stop running agents in-process — by
// running murtaugh-gateway instead of `murtaugh slack gateway`, which is a
// choice of binary rather than a mutation of data.
//
// It is also what makes this safe to run twice, and safe to run against a
// gateway that is live. Nothing an operator can do here takes their Murtaugh
// down.
//
// # What crosses, and the two that are genuinely BOTH
//
// #170's table is mostly unambiguous: profile bodies, MCP servers and scheduled
// jobs are the node's; Slack tokens, access, election timings, grants, workflow
// and unfurl rules are the gateway's.
//
// Two sections the table does not mention are read by both halves and are
// therefore copied rather than assigned:
//
// `chat` is the gateway's routing table AND the node's assignment rules —
// internal/nodeclaim derives a node's whole advertisement from chat.channels
// plus chat.defaults.agent, so a node without it claims nothing and answers
// nothing.
//
// `defaults` is read by the gateway for stream cadence, request timeouts and
// session lifetimes, and by the node for the ACP and approval halves. Splitting
// it either way silently changes behaviour on the side that lost it.
//
// # What must NOT cross, and why it cannot by accident
//
// Node token records and conversation pins live in side stores that Snapshot
// deliberately excludes (internal/config/nodetokens.go). Copying token hashes
// onto a node would put the fleet's whole credential table on somebody's laptop,
// and copying pins would give a node a stale opinion about delegations that are
// not its business. Because they are not in a Snapshot they cannot travel
// through this function at all — which is a property worth a test rather than a
// comment, and has one.

// nodeSections are the config_items sections a runtime node holds.
var nodeSections = []string{config.SectionAgent, config.SectionMCP, config.SectionJob}

// nodeSingletons are the singleton blocks a runtime node holds. Both are read by
// the gateway too; see the file comment.
var nodeSingletons = []string{config.SingletonChat, config.SingletonDefaults}

// SplitReport says what a split moved and what it left behind, per section.
//
// Both halves are reported because "what stayed on the gateway" is the question
// an operator asks second and the one a bare success message never answers.
type SplitReport struct {
	// Copied counts the rows written into the node store, keyed by section or
	// singleton key.
	Copied map[string]int
	// Kept counts the rows left on the gateway and not copied, same keys.
	Kept map[string]int
}

// Total is how many rows crossed.
func (r SplitReport) Total() int {
	total := 0
	for _, n := range r.Copied {
		total += n
	}
	return total
}

// SplitForNode copies the runtime node's half of a combined configuration into a
// second store.
//
// Both halves are validated afterwards, each under its OWN role, which is the
// check that makes the split meaningful rather than a file copy: the node store
// must load without the Slack credentials it will never hold, and the gateway
// store must still load now that a name it references may only be resolvable on
// a machine that is not connected yet.
//
// src is left untouched. See the file comment for why.
func SplitForNode(ctx context.Context, src, dst config.Store) (SplitReport, error) {
	snap, err := src.Snapshot(ctx)
	if err != nil {
		return SplitReport{}, fmt.Errorf("read the combined configuration: %w", err)
	}

	report := SplitReport{Copied: map[string]int{}, Kept: map[string]int{}}
	var forNode config.Snapshot
	for _, item := range snap.Items {
		if slices.Contains(nodeSections, item.Section) {
			forNode.Items = append(forNode.Items, item)
			report.Copied[item.Section]++
			continue
		}
		report.Kept[item.Section]++
	}
	for _, single := range snap.Singletons {
		if slices.Contains(nodeSingletons, single.Key) {
			forNode.Singletons = append(forNode.Singletons, single)
			report.Copied[single.Key]++
			continue
		}
		report.Kept[single.Key]++
	}

	if err := dst.Restore(ctx, forNode); err != nil {
		return SplitReport{}, fmt.Errorf("write the node configuration: %w", err)
	}

	// The node's half, under the node's rules: no Slack credentials required,
	// and every agent name it references resolved locally, because a node DOES
	// hold the bodies.
	if _, err := dst.Load(ctx, config.Config{Role: config.RoleNode}); err != nil {
		return SplitReport{}, fmt.Errorf("the node configuration is not valid: %w", err)
	}
	// The gateway's half, under the gateway's rules. The placeholder credentials
	// are the same trick internal/tools/cfg uses when validating store content:
	// the tokens are a bootstrap-file concern, not a database one, and this
	// function has not been handed a bootstrap file.
	gatewayBase := config.Config{
		Role:  config.RoleGateway,
		OAuth: config.OAuthConfig{AppToken: "x", BotToken: "x"},
	}
	if _, err := src.Load(ctx, gatewayBase); err != nil {
		return SplitReport{}, fmt.Errorf("the gateway configuration is not valid after the split: %w", err)
	}
	return report, nil
}
