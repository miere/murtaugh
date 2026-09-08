package nodeserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/tools"
)

// errLinkGone is what a tool call fails with when its connection ended
// underneath it. It is a sentinel because the note the model reads depends on
// it, and a string comparison there would rot the first time a wrapper was
// added.
var errLinkGone = errors.New("the connection to the gateway ended")

// droppedNote is the exact text a model is handed when a call dies with the
// link, and every clause in it is deliberate.
//
// There is NO idempotency information anywhere in Murtaugh's tool surface —
// tools.Tool is Name/Description/InputSchema/Invoke, and neither optional
// interface beside it says whether a call may be repeated. So the gateway may
// have executed this call, may have executed it partially, or may never have
// received it, and nothing on either side can tell the difference. That forces
// the policy: fail, retry nothing.
//
// The wording has to carry that to whoever reads the result. Hence, in order: it
// says the action MAY have taken effect (so a repeat is not obviously safe); it
// says do not retry, in the imperative, because "may not be safe to retry" reads
// as a caution rather than an instruction; and it tells the model what to do
// instead — say so — because a dead end with no alternative is the thing models
// route around. It follows the house style set by internal/tools/auth/request
// ("treat any error as a hard stop and do not retry the original call") and the
// approval gate's notes.
//
// # The wording is the backstop; the mechanism is the cancellation
//
// Be clear about which of the two actually stops a repeat, because it is not
// this string. A turn's context is a child of the server's, and Server.shutdown
// ends every turn — endTurn, hence t.cancel() — BEFORE it calls failCalls, which
// is what produces this note. So for a call made inside a turn, which is every
// call a node's agent makes, the turn is already cancelled by the time the note
// is handed back: an agent loop that checks its context between steps aborts and
// never reads it, let alone obeys it.
//
// What the wording covers is the caller whose context is NOT the turn's and so
// outlives the link — a headless or background caller holding a
// context.Background, and any agent loop that ignores cancellation. For those it
// is the only thing standing between a dropped call and a repeat of a tool that
// may have posted to Slack. It is a backstop, and a cheap one; it is not the
// guarantee.
const droppedNote = "The connection to Murtaugh's gateway dropped while this tool call was running. " +
	"The call may or may not have taken effect on the gateway, and there is no way to find out from here. " +
	"Do not retry it and do not try a different tool to work around it: tell the user the connection dropped mid-action and stop."

// unboundNote is the other abort case: no gateway is attached at all. Unlike
// droppedNote this one is certain — nothing was sent, so nothing ran — and it
// says so, because a model told "it may have happened" when it definitely did
// not is being made needlessly cautious.
const unboundNote = "Not run: this runtime node is not currently connected to Murtaugh's gateway, so the tool could not be reached. " +
	"Nothing happened. Tell the user the node is disconnected."

// ToolProxy is the node's stand-in for Murtaugh's own tools, which live on the
// gateway and hold the gateway's credentials.
//
// # Why a proxy and not a tunnel
//
// The obvious design is to tunnel the bytes of the MCP stream an acp agent
// already speaks. #170 Concern 5 explains why that is hard: the gateway end is
// an MCP server session whose entire state IS the byte stream, so a reconnected
// pipe is dead rather than degraded, and the node would have to own initialise
// replay and request-id rewriting.
//
// Proxying per CALL removes the problem instead of solving it. The MCP session
// stays where it already is — on the node, over a local unix socket, between the
// agent's subprocess and the node's own aggregator — so it never reconnects, so
// there is nothing to replay. What crosses the network is one request and one
// answer, on a link that already sequences, acknowledges and orders frames.
//
// It also fixes the backend a tunnel would have missed: a NATIVE agent, the
// default one, never touches the aggregator at all. It resolves a []tools.Tool
// and calls Invoke. A tunnel would have restored tools for acp and claude_code
// and left native silently tool-less.
//
// # Unbound, then bound
//
// It is built before any connection exists, because the agent's backends want a
// registry at construction and a native agent latches its toolset at first
// Initialize. Same shape as ToolGate and BackgroundSink. Unbound it FAILS every
// call with a legible note rather than vanishing: a tool that disappears between
// reconnects changes the model's tool list underneath it, which is a harder
// thing to debug than a tool that answers "the node is disconnected".
type ToolProxy struct {
	log      *slog.Logger
	registry *tools.Registry

	mu     sync.Mutex
	server *Server
	tools  map[string]*proxyTool
}

// NewToolProxy returns an unbound proxy and the empty registry it will fill.
func NewToolProxy(log *slog.Logger) *ToolProxy {
	if log == nil {
		log = slog.Default()
	}
	return &ToolProxy{log: log, registry: tools.NewRegistry(), tools: make(map[string]*proxyTool)}
}

// Registry is the tool registry to hand the node's runtime builder. It is the
// real thing, populated at the first handshake and stable thereafter.
func (p *ToolProxy) Registry() *tools.Registry { return p.registry }

func (p *ToolProxy) bind(s *Server) {
	p.mu.Lock()
	p.server = s
	p.mu.Unlock()
}

func (p *ToolProxy) unbind(s *Server) {
	p.mu.Lock()
	if p.server == s {
		p.server = nil
	}
	p.mu.Unlock()
}

func (p *ToolProxy) bound() *Server {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.server
}

// refresh asks the gateway what this node may reach and republishes it.
//
// A name seen for the first time is registered; a name already registered has
// its metadata updated in place. Registering is one-way on purpose:
// tools.Registry panics on a duplicate, and — more to the point — a native
// agent's toolset is latched at its first Initialize, so a tool withdrawn on a
// later reconnect would still be in the agent's list. Updating the descriptor
// keeps the schema and description honest; the SET is frozen at the first
// handshake, and a gateway whose partition changes needs the node restarted.
// That is a known limit of this item and not a hidden one.
func (p *ToolProxy) refresh(ctx context.Context, s *Server) error {
	response, err := s.callGateway(ctx, agentwire.MethodToolList, agentwire.Empty{})
	if err != nil {
		return err
	}
	var list agentwire.ToolListResult
	if err := response.Into(&list); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	added, refreshed := 0, 0
	for _, descriptor := range list.Tools {
		if descriptor.Name == "" {
			continue
		}
		schema, err := decodeSchema(descriptor.InputSchema)
		if err != nil {
			p.log.Warn("nodeserve: a gateway tool arrived with an unreadable schema and was dropped",
				"tool", descriptor.Name, "error", err)
			continue
		}
		if existing, ok := p.tools[descriptor.Name]; ok {
			existing.update(descriptor, schema)
			refreshed++
			continue
		}
		t := &proxyTool{proxy: p, descriptor: descriptor, schema: schema}
		p.tools[descriptor.Name] = t
		p.registry.Register(t)
		added++
	}
	p.log.Info("murtaugh tools available from the gateway", "added", added, "refreshed", refreshed, "total", len(p.tools))
	return nil
}

// Call runs one of the gateway's tools by name.
//
// It is what every proxy tool's Invoke does, exported because the name is the
// interesting part: a name this node was never offered is refused BY THE
// GATEWAY, not filtered here. The node-side registry is a convenience for the
// agent's toolset; the boundary is on the other end of the link, and it has to
// be, because a node admin owns their node's configuration.
func (p *ToolProxy) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	server := p.bound()
	if server == nil {
		return unboundNote, nil
	}
	// The stream id is what lets the gateway put the turn's Slack location back
	// on the context before it invokes. Without it the gateway's approval gate
	// short-circuits to allowed with no card, and `ask`/`present_plan` degrade
	// to non-interactive — a silent ungating rather than a failure.
	//
	// KNOWN LIMIT, named here rather than left to be found as "`ask` works on one
	// backend": only a NATIVE agent's call carries it. A native tool call runs
	// under the turn context servePrompt built, which withStream tagged. An
	// acp/claude_code call arrives instead through this node's local MCP
	// aggregator, whose context mcpbridge decorates once per SESSION — it cannot
	// name a turn, because a session outlives one. Those two backends therefore
	// call with an empty stream: `ping` and `version` are unaffected, and `ask`
	// and `present_plan` fail gateway-side for want of a thread to render into.
	// Closing it means carrying a per-turn id onto the aggregator's per-call
	// context, which is its own item.
	stream, _ := streamOf(ctx)
	response, err := server.callGateway(ctx, agentwire.MethodToolCall, agentwire.ToolCall{
		Stream: stream,
		Name:   name,
		Args:   args,
	})
	switch {
	case errors.Is(err, errLinkGone):
		// Not an error return: an error is rendered to the model as a failure it
		// is free to reinterpret, whereas this note has to be read as an
		// instruction. See droppedNote.
		return droppedNote, nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "", fmt.Errorf("the turn ended before %s finished on the gateway; the call was abandoned and must not be retried: %w", name, err)
	case err != nil:
		return "", err
	}
	var result agentwire.ToolResult
	if err := response.Into(&result); err != nil {
		return "", err
	}
	return result.Content, nil
}

// proxyTool is one gateway tool republished into the node's registry.
//
// It reimplements exactly one of the optional tool interfaces — MCPNamer — and
// deliberately implements neither approval interface. Both of those are
// consulted by type assertion where the tool RUNS, and a proxied call runs on
// the gateway: the gateway holds the real tool, so it can classify per call from
// the arguments, and the human it would ask is the gateway's user in either
// case. Implementing them here as well would prompt one person twice for one
// call. MCPNamer is different because it is consumed HERE, when the node's own
// aggregator publishes the tool to the agent: dropping it republishes `ask` as
// `ask` rather than AskUserQuestion, and a Claude Code agent then simply stops
// asking, with nothing anywhere saying why.
type proxyTool struct {
	proxy *ToolProxy

	mu         sync.Mutex
	descriptor agentwire.ToolDescriptor
	schema     *jsonschema.Schema
}

func (t *proxyTool) update(descriptor agentwire.ToolDescriptor, schema *jsonschema.Schema) {
	t.mu.Lock()
	t.descriptor = descriptor
	t.schema = schema
	t.mu.Unlock()
}

func (t *proxyTool) Name() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.descriptor.Name
}

func (t *proxyTool) Description() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.descriptor.Description
}

func (t *proxyTool) InputSchema() *jsonschema.Schema {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.schema
}

// MCPName carries the gateway tool's published-name override across.
func (t *proxyTool) MCPName() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.descriptor.PublishedName
}

func (t *proxyTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	return t.proxy.Call(ctx, t.Name(), args)
}

func decodeSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	return &schema, nil
}

var _ tools.Tool = (*proxyTool)(nil)
