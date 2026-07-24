package acp

import "encoding/json"

// AgentCapabilities captures the parts of an ACP agent's initialize response
// that govern how Murtaugh may talk to it — chiefly which MCP server transports
// it accepts in session/new. Stdio is always available (mandatory in ACP); HTTP
// and SSE are only honoured when the agent advertises them. Note: an advertised
// transport is necessary but not sufficient — at least one shipping agent
// advertises http while silently dropping http servers, so any future HTTP path
// must verify a connection actually formed rather than trust this flag.
type AgentCapabilities struct {
	ProtocolVersion int
	MCP             MCPCapabilities
	LoadSession     bool
}

// MCPCapabilities reports which url-based MCP server transports the agent
// accepts in session/new (beyond the mandatory stdio).
type MCPCapabilities struct {
	HTTP bool
	SSE  bool
}

// Capabilities returns what the agent advertised at initialize. Zero value until
// Initialize completes.
func (c *ProcessClient) Capabilities() AgentCapabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caps
}

// parseAgentCapabilities decodes the subset of an ACP initialize response that
// Murtaugh acts on. Missing fields decode to their zero value (stdio-only), the
// safe default. An unparseable result yields zero capabilities rather than an
// error: the handshake already succeeded, and stdio — all Murtaugh needs today —
// is always available.
func parseAgentCapabilities(result json.RawMessage) AgentCapabilities {
	var decoded struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			LoadSession     bool `json:"loadSession"`
			MCPCapabilities struct {
				HTTP bool `json:"http"`
				SSE  bool `json:"sse"`
			} `json:"mcpCapabilities"`
		} `json:"agentCapabilities"`
	}
	if len(result) > 0 {
		_ = json.Unmarshal(result, &decoded)
	}
	return AgentCapabilities{
		ProtocolVersion: decoded.ProtocolVersion,
		MCP: MCPCapabilities{
			HTTP: decoded.AgentCapabilities.MCPCapabilities.HTTP,
			SSE:  decoded.AgentCapabilities.MCPCapabilities.SSE,
		},
		LoadSession: decoded.AgentCapabilities.LoadSession,
	}
}
