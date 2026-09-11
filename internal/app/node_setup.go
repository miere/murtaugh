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

func (a *Application) agentReferences() []config.AgentReference {
	if refs := a.agentRefs.Load(); refs != nil {
		return *refs
	}
	return nil
}

func (a *Application) publishAgentReferences(cfg config.Config) {
	refs := cfg.AgentReferences()
	a.agentRefs.Store(&refs)
}

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
