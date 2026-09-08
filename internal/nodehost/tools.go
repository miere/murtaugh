package nodehost

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agent/remote"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/tools"
	"github.com/miere/murtaugh/internal/toolset"
)

// toolServer is the ONE place the tool partition is enforced.
//
// Both entry points consult it. tool.list never names a tool the node may not
// call, and tool.call re-checks by name before looking anything up — because a
// node that asks for something it was not offered must be REFUSED by the
// gateway, not filtered by the node. A node admin owns their node's config and
// can put any family they like in an agent's `tools:`; the node is therefore not
// a place a trust decision can live.
type toolServer struct {
	host *Host
	log  *slog.Logger
}

// List answers tool.list with the node-reachable slice of the gateway's
// registry.
//
// A gateway with no registry bound FAULTS rather than answering an empty list.
// Empty is a legitimate answer meaning "you may reach nothing", and a node that
// could not tell the two apart would publish an agent with a silently empty
// toolset — which is exactly the shape of #185's failure and of the regression
// this item exists to prevent. Faulting makes the node refuse to come up, and
// its redial loop retries a second later, by which time the gateway has finished
// building.
func (s *toolServer) List(context.Context) ([]agentwire.ToolDescriptor, error) {
	registry := s.host.registry()
	if registry == nil {
		return nil, fmt.Errorf("nodehost: this gateway has no tool registry bound yet; retry the handshake")
	}
	var out []agentwire.ToolDescriptor
	for _, t := range registry.All() {
		if !toolset.NodeMayReach(t.Name()) {
			continue
		}
		descriptor, err := describe(t)
		if err != nil {
			// One tool that cannot be described is dropped; the agent keeps the
			// rest of its toolset. Same contract as toolset.Problem: a
			// precondition that fails for one tool degrades that tool.
			s.log.Warn("nodehost: a tool could not be described for a node", "tool", t.Name(), "error", err)
			continue
		}
		out = append(out, descriptor)
	}
	return out, nil
}

// Call runs one tool on the gateway for a node's agent.
//
// Three things happen here that the in-process path gets for free:
//
//   - The partition is re-checked. See the type doc.
//   - The turn's agent.TurnLocation is put back on the context. Without it the
//     approval gate short-circuits to ALLOWED with no card — its documented
//     headless behaviour — and `ask`, `present_plan` and `auth.request` all
//     silently stop being interactive. Losing it does not fail; it ungates.
//   - The approval gate runs, once, HERE. The node deliberately does not gate a
//     proxied call: the human being asked is the gateway's user either way, and
//     gating on both sides would prompt one person twice for one call.
//
// What does NOT cross is agent.TurnEnv — the agent profile's environment, which
// in process makes a bridged tool shell out as the agent rather than as the
// daemon. After the split the profile lives on the node and the tool runs on the
// gateway, so a gateway-executed tool call runs with the GATEWAY's environment.
// That is a real behaviour change and it is stated here rather than discovered:
// it is also why `auth.request` is classified ReachLocal.
func (s *toolServer) Call(ctx context.Context, call agentwire.ToolCall) (agentwire.ToolResult, error) {
	name := strings.TrimSpace(call.Name)
	if reach, why := toolset.ReachOf(name); reach != toolset.ReachNode {
		s.log.Warn("nodehost: a node asked for a tool it may not reach", "tool", name, "reason", why)
		return agentwire.ToolResult{}, fmt.Errorf("the gateway does not serve %q to a runtime node (%s)", name, why)
	}
	registry := s.host.registry()
	if registry == nil {
		return agentwire.ToolResult{}, fmt.Errorf("nodehost: this gateway has no tool registry bound")
	}
	tool, ok := registry.Get(name)
	if !ok {
		return agentwire.ToolResult{}, fmt.Errorf("the gateway has no tool named %q", name)
	}

	ctx = s.host.locate(ctx, call.Stream)
	if denied, note := s.gate(ctx, tool, call.Args); denied {
		// A denial is a RESULT, not a fault. The note is the call's result
		// string handed to the model, which is what both in-process paths do —
		// and the model must be able to tell "the user said no" apart from "the
		// call broke", because those imply different next moves.
		return agentwire.ToolResult{Content: note}, nil
	}

	result, err := tool.Invoke(ctx, call.Args)
	if err != nil {
		return agentwire.ToolResult{}, err
	}
	return agentwire.ToolResult{Content: render(result)}, nil
}

// gate runs the gateway's human approval for a proxied call. It is a port of
// internal/frontends/mcp's gate, reading the same two optional tool interfaces,
// because the classification is per call and depends on the ARGUMENTS — so it
// can only be done on the side that holds the tool.
func (s *toolServer) gate(ctx context.Context, tool tools.Tool, args map[string]any) (denied bool, note string) {
	approve := s.host.approver()
	if approve == nil {
		return false, ""
	}
	classifier, ok := tool.(tools.ApprovalClassifier)
	if !ok || !classifier.RequiresApproval(args) {
		return false, ""
	}
	summary := tool.Name()
	if summarizer, ok := tool.(tools.ApprovalSummarizer); ok {
		summary = summarizer.ApprovalSummary(args)
	}
	allowed, n := approve(ctx, tool.Name(), summary)
	if !allowed {
		return true, n
	}
	return false, ""
}

// mcpNamer is internal/frontends/mcp's MCPNamer, declared structurally rather
// than imported — the same idiom internal/agent uses for Aggregator. It keeps
// this package's dependency footprint at the tool abstraction and off the MCP
// frontend, which matters because this is the package the gateway binary links.
type mcpNamer interface{ MCPName() string }

// describe renders one tool for the wire.
func describe(t tools.Tool) (agentwire.ToolDescriptor, error) {
	descriptor := agentwire.ToolDescriptor{Name: t.Name(), Description: t.Description()}
	if namer, ok := t.(mcpNamer); ok {
		descriptor.PublishedName = strings.TrimSpace(namer.MCPName())
	}
	if schema := t.InputSchema(); schema != nil {
		encoded, err := json.Marshal(schema)
		if err != nil {
			return agentwire.ToolDescriptor{}, fmt.Errorf("encode input schema: %w", err)
		}
		descriptor.InputSchema = encoded
	}
	return descriptor, nil
}

// render turns a tool's result into the string the node hands back to its
// agent. Strings pass through so trivial tools stay uncluttered; everything else
// is JSON. It is the same convention internal/frontends/mcp applies, so a
// proxied call and a local one read identically to a model.
func render(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(encoded)
}

// locate puts the turn's Slack location back on the context.
//
// The stream id is the only correlation available: the gateway minted it for the
// prompt, and remote.Client already records the location against it because
// answerApproval needed exactly this. An unknown stream leaves the context bare,
// which is a visible degradation rather than a silent one — every tool that
// needs the location refuses or degrades in a way the user can see, and the
// warning below says which call it was.
//
// An EMPTY stream is not an anomaly today: only a native agent's call carries
// one. See nodeserve.ToolProxy.Call, which explains why an acp/claude_code call
// cannot name its turn yet and what that costs.
func (h *Host) locate(ctx context.Context, stream string) context.Context {
	if stream == "" {
		return ctx
	}
	client, err := h.client()
	if err != nil {
		return ctx
	}
	location, ok := client.StreamLocation(stream)
	if !ok {
		h.log.Warn("a node's tool call named a turn this gateway does not know; it will run without a thread to ask in", "stream", stream)
		return ctx
	}
	return agent.WithTurnLocation(ctx, location)
}

func (h *Host) registry() *tools.Registry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.tools
}

func (h *Host) approver() func(context.Context, string, string) (bool, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.approve
}

// SetTools binds the registry a node's agent reaches over the tool channel.
//
// It is set here, from the runtime builder, because that is where the gateway
// hands its registry over, and a reload runs the builder again while the node
// connection survives. Do not read more into that than is there: the registry
// itself is built exactly ONCE, in app.New, and buildGateway passes the same
// pointer on every reload, so what a reload re-binds is an identical pointer.
//
// The staleness that implies runs the OTHER way, and is not handled. The
// registry's tools close over their credentials at construction — the bot token
// among them — so nothing rebuilds them and a node's proxied slack.* calls keep
// using the token the process started with. Rotating it needs the gateway
// restarted. That is a limit of the registry's lifetime, not of this binding,
// and it is written here because this is where someone would look for it.
func (h *Host) SetTools(registry *tools.Registry) {
	h.mu.Lock()
	h.tools = registry
	h.mu.Unlock()
}

var _ remote.ToolHost = (*toolServer)(nil)
