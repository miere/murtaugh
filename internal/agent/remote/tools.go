package remote

import (
	"context"
	"errors"
	"fmt"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/nodelink"
)

// This file is the gateway's half of the tool channel: the node → gateway
// request direction, which until now was a stub that answered every call with
// "the gateway serves no %q request".
//
// It stays at agentwire, not at internal/tools. What a node may reach is a trust
// decision and it is made in internal/nodehost, which holds the registry and the
// partition; this package only carries the frames and owns their lifetime. The
// split matters because the lifetime rules are the subtle part and they belong
// with the link, not with the policy.

// ToolHost answers a node's tool requests. internal/nodehost implements it over
// the gateway's registry and toolset.Reach.
//
// Call MUST honour ctx: this package cancels it when the link dies, and that
// cancellation is the only thing standing between a dropped node and a tool
// invocation — possibly one parked on a ten-minute approval card — that runs to
// completion for an agent which no longer exists.
type ToolHost interface {
	List(ctx context.Context) ([]agentwire.ToolDescriptor, error)
	Call(ctx context.Context, call agentwire.ToolCall) (agentwire.ToolResult, error)
}

// serveRequest answers one request the node made.
//
// It runs on its own goroutine, never on the read loop. nodelink acknowledges a
// frame only once the handler RETURNS, and a tool call can block on a human for
// ten minutes; answering inline would stall acknowledgement for the whole link,
// so every conversation on that node would go quiet, not just the one whose tool
// was waiting.
func (c *Client) serveRequest(msg agentwire.Message) {
	switch msg.Method {
	case agentwire.MethodToolList:
		c.serveToolList(msg)
	case agentwire.MethodToolCall:
		c.serveToolCall(msg)
	case agentwire.MethodAdvertise:
		c.serveAdvertise(msg)
	default:
		c.rejectRequest(msg)
	}
}

func (c *Client) serveToolList(msg agentwire.Message) {
	if c.tools == nil {
		c.answer(msg.ID, nil, errors.New("remote: this gateway serves no tools to nodes"))
		return
	}
	ctx, done := c.trackInbound(msg.ID)
	defer done()
	list, err := c.tools.List(ctx)
	if err != nil {
		c.answer(msg.ID, nil, err)
		return
	}
	c.answer(msg.ID, agentwire.ToolListResult{Tools: list}, nil)
}

func (c *Client) serveToolCall(msg agentwire.Message) {
	if c.tools == nil {
		c.answer(msg.ID, nil, errors.New("remote: this gateway serves no tools to nodes"))
		return
	}
	var call agentwire.ToolCall
	if err := msg.Into(&call); err != nil {
		c.answer(msg.ID, nil, err)
		return
	}
	ctx, done := c.trackInbound(msg.ID)
	defer done()
	result, err := c.tools.Call(ctx, call)
	if err != nil {
		c.answer(msg.ID, nil, err)
		return
	}
	c.answer(msg.ID, result, nil)
}

// trackInbound registers an in-flight node request so the link's death cancels
// it, and returns the context to run it under.
//
// The map is separate from c.pending on purpose. Both sides mint request ids
// from independent counters starting at "1", and the invariant that saves us is
// that each side only ever looks up ids it minted itself. One shared map would
// turn two unrelated live requests both numbered "1" into one.
func (c *Client) trackInbound(id string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		return ctx, func() {}
	}
	c.inbound[id] = cancel
	c.mu.Unlock()
	return ctx, func() {
		c.mu.Lock()
		delete(c.inbound, id)
		c.mu.Unlock()
		cancel()
	}
}

// cancelInbound cancels every tool call this gateway is running for the node.
//
// It is called when the link dies and when the client closes, and it is the
// gateway-side half of the no-retry abort policy: the node will already have
// failed the call for its model, so anything still running here is running for
// nobody. Not cancelling it is the known gap #170 names — a drop mid-approval
// executing a side effect for an agent that no longer exists.
//
// It fixes that gap only for THIS path, where the context is ours end to end.
// The in-process aggregator cannot be fixed the same way; see the note in
// internal/nodehost's package doc.
func (c *Client) cancelInbound() {
	c.mu.Lock()
	pending := c.inbound
	c.inbound = make(map[string]context.CancelFunc)
	c.mu.Unlock()
	for _, cancel := range pending {
		cancel()
	}
}

// answer sends a result or a fault for a node-minted request id.
func (c *Client) answer(id string, body any, failure error) {
	var msg agentwire.Message
	if failure != nil {
		msg = agentwire.Fault(id, failure)
	} else {
		encoded, err := agentwire.Result(id, body)
		if err != nil {
			msg = agentwire.Fault(id, fmt.Errorf("remote: encode tool answer: %w", err))
		} else {
			msg = encoded
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), toolAnswerTimeout)
	defer cancel()
	// A closed link is not logged: the node going away while its own call was
	// still running is the state the abort policy is designed around, not an
	// anomaly, and it happens once per tool call in flight at the drop.
	if err := c.send(ctx, msg); err != nil && !errors.Is(err, nodelink.ErrLinkClosed) {
		c.log.Warn("remote: deliver tool answer to the node", "error", err, "id", id)
	}
}
