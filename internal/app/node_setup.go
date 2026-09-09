package app

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/onboarding"
	gateway "github.com/miere/murtaugh/internal/slack/gateway"
)

// This file wires #170 Change I's onboarding trigger across the three packages
// that each hold one third of it.
//
// internal/nodehost knows a node attached with nothing configured, and who owns
// it — but has no Slack. internal/slack/gateway owns the form — but knows
// nothing about nodes. And the profiles the form produces have to reach the
// NODE's store, which is a round trip over a connection only nodehost holds. So
// the composition root is where the three meet, as it is for the gateway's own
// agent setup next door in agent_setup.go.

// wireNodeOnboarding connects the node registry to the setup form.
//
// Called once. Both closures reach the CURRENT gateway through the holder rather
// than a captured pointer, for the reason the election's callbacks do: a
// configuration reload replaces the gateway, and a trigger bound to a torn-down
// predecessor would post nothing and report no error.
func (a *Application) wireNodeOnboarding(holder *gatewayHolder) {
	if a.nodeEndpoint.Onboard == nil {
		return
	}
	a.nodeEndpoint.Onboard(NodeOnboarding{
		References: a.agentReferences,
		Unconfigured: func(ctx context.Context, nodeID, userID string) {
			holder.get().OfferNodeSetup(ctx, nodeID, userID)
		},
		Settled: func(nodeID, userID string) {
			holder.get().WithdrawNodeSetup(nodeID, userID)
		},
	})
}

// agentReferences is every agent profile name the running configuration
// mentions.
//
// It is served from a snapshot published by attachAgentSetup rather than read
// off a.cfg, because it is called from a node's connection goroutine while a
// reload may be replacing that field. The snapshot is refreshed on every reload,
// because attachAgentSetup runs for every rebuilt gateway.
func (a *Application) agentReferences() []config.AgentReference {
	if refs := a.agentRefs.Load(); refs != nil {
		return *refs
	}
	return nil
}

// publishAgentReferences records what the current configuration names.
func (a *Application) publishAgentReferences(cfg config.Config) {
	refs := cfg.AgentReferences()
	a.agentRefs.Store(&refs)
}

// newNodeProfileWriter builds the closure that sends a completed form to a node.
//
// It is the mirror of newAgentProfileWriter, and the differences are the
// interesting part. Nothing is written HERE: the store the profiles belong in is
// on the node's machine, so this translates and sends, and the node decides. The
// .env credential travels with them rather than being written locally, because
// the profile that references it will be built over there. And there is no
// reload, because the node restarts instead — both agent backend families latch
// their toolset at construction, so a node process that came up with no agent
// cannot grow one.
func (a *Application) newNodeProfileWriter() gateway.NodeProfileWriter {
	return func(ctx context.Context, nodeID string, profiles onboarding.Profiles) error {
		if a.nodeEndpoint.Configure == nil {
			return fmt.Errorf("this build cannot configure a runtime node")
		}
		wire, err := nodeConfiguration(profiles)
		if err != nil {
			return err
		}
		result, err := a.nodeEndpoint.Configure(ctx, nodeID, wire)
		if err != nil {
			return err
		}
		a.logger.Info("configured a runtime node from the setup form",
			"node_id", nodeID, "profiles", result.Applied, "restarting", result.Restarting)
		return nil
	}
}

// nodeConfiguration turns a completed form into the wire body.
//
// The tweaker profile's work_dir is deliberately left as the form produced it —
// empty, because onboarding.Build was given no config dir — and the node fills
// it with its own. That directory is on the node's machine and the gateway has
// no way to know it, so there is nothing for this side to send: see the note in
// agentwire.NodeConfiguration for why a field carrying the empty answer would
// only look like a contract.
func nodeConfiguration(profiles onboarding.Profiles) (agentwire.NodeConfiguration, error) {
	agents := make(map[string]json.RawMessage, 2)
	for name, profile := range map[string]any{
		profiles.Name:          profiles.Default,
		onboarding.TweakerName: profiles.Tweaker,
	} {
		body, err := json.Marshal(profile)
		if err != nil {
			return agentwire.NodeConfiguration{}, fmt.Errorf("encode the %q agent: %w", name, err)
		}
		agents[name] = body
	}
	chat, err := json.Marshal(profiles.Chat)
	if err != nil {
		return agentwire.NodeConfiguration{}, fmt.Errorf("encode the chat block: %w", err)
	}
	out := agentwire.NodeConfiguration{Agents: agents, Chat: chat}
	if profiles.EnvKey != "" {
		out.Env = map[string]string{profiles.EnvKey: profiles.EnvValue}
	}
	return out, nil
}
