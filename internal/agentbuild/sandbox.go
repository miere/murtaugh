package agentbuild

import (
	"fmt"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agent/sandbox"
	"github.com/miere/murtaugh/internal/config"
)

// resolveSandbox turns an agent's `sandbox:` block into the confinement its
// process backend applies. It returns a nil agent.Sandbox for a native agent (no
// process to confine) and for mode off.
func resolveSandbox(profile config.AgentProfile, resolved ResolvedAgent, deps Deps) (agent.Sandbox, error) {
	if resolved.Kind == config.AgentKindNative {
		return nil, nil
	}

	var socket string
	if deps.Bridge != nil {
		socket = deps.Bridge.SocketPath()
	}

	plan, err := sandbox.Resolve(sandbox.Spec{
		Mode:          profile.ResolvedSandboxMode(),
		WorkDir:       resolved.Dir(),
		Write:         profile.Sandbox.Write,
		DenyRead:      profile.Sandbox.DenyRead,
		EnvAllow:      profile.Sandbox.Env,
		BridgeSocket:  socket,
		NodeTokenPath: deps.NodeTokenPath,
	})
	if err != nil {
		return nil, fmt.Errorf("agentbuild: agent %q: %w", resolved.Name(), err)
	}
	if plan == nil {
		// Explicit nil-check, not a direct assignment: storing a nil *sandbox.Plan
		// in the interface would produce a non-nil agent.Sandbox and silently send
		// every unsandboxed agent down the confined path.
		return nil, nil
	}
	return plan, nil
}
