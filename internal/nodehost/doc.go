// Package nodehost is the gateway's inbound edge for runtime nodes: the one
// endpoint a node dials, the one place a node token is verified, and the slot
// the resulting agent.Client lives in.
//
// # This is the daemon's first listener
//
// Nothing in Murtaugh has ever bound a port. The only listener in the tree is
// the MCP bridge's unix socket, and the Slack connection is outbound socket
// mode. So this package does not merely add an endpoint to an existing server —
// it introduces the act of listening, and with it a new attack surface on the
// machine. That is why it is reached only from cmd/murtaugh-gateway, only when
// an address is passed explicitly, and never from `murtaugh slack gateway`,
// which remains the shipping default and binds nothing.
//
// # One node is the whole world
//
// There is no registry (#195), no delegation (#196) and no failover (#197), so
// this holds exactly one attached node and every configured agent name resolves
// to it. A second node replaces the first. That is a deliberate simplification
// for #193 — the item exists to find out whether a turn survives the hop, and
// an addressing scheme invented here would be replaced by item 9 before it was
// used. Everything that will become per-node state is behind one mutex and one
// struct, which is the shape a map is grown from.
//
// # The tool channel, and the one place the partition is enforced
//
// A node's agent reaches Murtaugh's own tools through this package, over the
// connection the node dialled — the gateway still never dials a node. The frames
// are agentwire's tool.list and tool.call, carried by internal/agent/remote;
// what they may name is decided here, in tools.go, against toolset.Reach.
//
// It is enforced in ONE place, and on BOTH verbs. tool.list never mentions a
// tool a node may not have, and tool.call re-checks the name before looking
// anything up. The second check is not redundant: the list is a convenience for
// the node's agent, while the check is the boundary. A node admin owns their
// node's configuration and can put any family they like in an agent's `tools:`,
// so a node cannot be trusted to filter itself — a node that asks for a tool it
// was not offered is REFUSED, not quietly ignored.
//
// Two things a proxied call does that an in-process one gets for free, both of
// which fail silently if forgotten. The turn's agent.TurnLocation is put back on
// the context, because without it the approval gate short-circuits to ALLOWED
// with no card and `ask`/`present_plan` degrade to non-interactive. And the
// approval gate runs HERE and only here — the node does not gate a proxied call,
// because the human is the gateway's user either way and gating twice would
// prompt one person twice for one action.
//
// What does NOT cross is agent.TurnEnv, the agent profile's environment. After
// the split the profile lives on the node and the tool runs on the gateway, so a
// gateway-executed tool call runs with the GATEWAY's environment. That is a real
// behaviour change, and it is why auth.request is classified as meaningless
// remotely rather than merely denied.
//
// # The in-flight-handler gap: closed here, still open in process
//
// #170 names a gap: the gateway's MCP frontend does not cancel an in-flight tool
// handler when its connection dies, so with a ten-minute approval timeout a drop
// mid-approval can execute a side effect for an agent that no longer exists.
//
// On THIS path it is closed. Every tool call a node makes runs under a context
// this package owns, registered in remote.Client and cancelled when the link
// dies — from watchLink when the node vanishes, and from Close when the gateway
// drops it. internal/nodehost's own tests prove the cancellation lands.
//
// It is NOT closed on the in-process path, and it cannot be closed the same way,
// which is worth writing down because the obvious fix looks like it would work
// and does not. The MCP Go SDK wraps a connection's context in an internal
// notDone type whose Done() returns nil and whose Err() returns nil, forever
// (jsonrpc2/conn.go), and every request context descends from it. So no ancestor
// context reaches a tool handler: cancelling the context handed to
// mcpbridge's server cancels nothing, and a test written against it would pass
// because the handler finished on its own. The only in-band cancel is a
// notifications/cancelled frame from the client — which requires the connection
// that just died. Closing it properly means an out-of-band per-call abort inside
// internal/mcpbridge, which is its own item and not this one.
//
// # Verification happens before the upgrade
//
// nodetoken.Verify is called on the HTTP request, and the socket is upgraded
// only if it passes. An upgrade first would hand an unauthenticated peer a
// connection to hold, and the rejection would arrive as a WebSocket close frame
// rather than as an HTTP status the dialler can read and print.
//
// The Host also implements nodetoken.ConnectionCloser, which is what makes
// revocation mean what #170 says it means: revoking a credential closes the
// connections it authenticated, rather than only refusing the next handshake.
// It closes by SELECTOR, never by node — during a rotation a node holds two
// live credentials and closing "every connection of node X" would drop it in
// the middle of the operation the overlap exists to make seamless.
package nodehost
