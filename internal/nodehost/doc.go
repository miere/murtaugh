// Package nodehost is the gateway's inbound edge for runtime nodes: the one
// endpoint a node dials, the one place a node token is verified, and the
// registry the resulting connections live in.
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
// # The registry: who is connected, and what each one claims
//
// A node's identity comes from the CREDENTIAL it presented, never from anything
// it said — there is no node id anywhere in the wire format, because inside a
// fleet a node that announces who it is can announce somebody else. What a node
// does say is its ADVERTISEMENT: the agent profile names it serves and the
// channels it claims an assignment rule for. It rides the handshake answer at
// connect and arrives as node.advertise on every later change, so the gateway
// matches locally against a cached copy and never asks a node anything at
// delegation time. That is #170's push-not-poll rule, and the reason for it is
// that an N-way fan-out on the first message of every conversation lets one
// wedged node add a timeout to every delegation in the workspace. The cost is a
// staleness window one configuration edit wide.
//
// The registry is IN MEMORY, deliberately. #170's table puts it in the
// gateway's column, which says who owns it and not where it is written down; an
// entry is a live socket plus a claim, and both die with the process. Only the
// elected leader accepts node connections, so a persisted registry read by a
// standby is guaranteed stale. The half that genuinely outlives a process is the
// conversation PIN, which #170 says to store and item 10 does, as a side store
// in the family internal/config/nodetokens.go established.
//
// # A connection is the unit; a node is what you ask about
//
// The map is keyed per connection because #170 requires two credentials to be
// valid at once so a token rotates with no downtime, and CloseCredential closes
// by selector so revoking the old one does not drop the node. Keying by node id
// would make a rotation's second connection evict the first. Nodes() collapses
// the other way, to one entry per node id, because delegation must never see one
// machine twice and round-robin it against itself.
//
// What is NOT here is the CHOICE. A new conversation opens on the most recently
// attached node, because picking between them properly needs a conversation
// key, a fleet and a stored pin — item 10. Writing a choice here would have
// meant writing one to be replaced before it was ever relied on.
//
// What IS here is the BINDING, and it could not wait for the choice. The
// session a conversation opens is bound to the connection that minted it, and
// every later prompt, cancel and close for that session goes back to that
// connection. Without it a second node attaching would silently take over every
// live conversation on the first — session ids are minted per node, so the
// newcomer is handed ids it never issued: a prompt fails in front of the user,
// and a cancel is answered as success while the turn runs on unimpeded and the
// gateway blocks draining a stream that will never close. A session whose
// connection has gone is agent.ErrSessionGone, distinct from ErrNoNode because
// it means THIS session cannot run while a new one could; re-opening it
// elsewhere is item 10's re-election.
//
// # What a node is not allowed to say
//
// An advertisement carries match patterns and profile names. It does not carry
// `allow_anyone`, which waives the gateway's own access list for a channel's
// chat surface: a node admin — possibly a guest holding a grant — writes that
// file, and letting it cross would let them open the gateway to the whole
// workspace from their laptop. It does not carry `reply_on_thread`, which
// decides the conversation key a pin is keyed by. The rule is the one
// internal/toolset/partition.go states for tools: enforcement is gateway-side
// because a node cannot be trusted to filter itself. internal/nodeclaim is
// where the node-side half of that is applied, and agentwire.Advertisement's
// shape is what makes the rule checkable by reading a struct.
//
// # A node that goes quiet is still "connected"
//
// nodesocket sets no read deadline and there is no ping/pong; the link's
// keepalive is the node's own AckInterval. A laptop that sleeps without sending
// a FIN therefore stays in this registry until a write to it fails at the
// transport's write timeout or TCP gives up. That is a real property of the
// registry and not a bug in it, and delegation has to expect a node that is
// listed and unreachable.
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
//
// One revocation hole is open and is named rather than left to be found: a
// connection whose handshake is in flight when its credential is revoked is
// verified before it is published, so it attaches after the sweep and nothing
// closes it. The ordering is unchanged since item 7; closing it means
// re-checking the credential at publication.
//
// # Nothing about a node is announced
//
// Attach, detach, revocation and every change to what a node claims go to the
// journal, on the gateway stream, as kind `node`. #170 is explicit that
// disconnects are journalled and not announced, because a laptop sleeping at
// six o'clock disconnects every evening and a nightly message trains the admin
// to ignore the one that matters.
package nodehost
