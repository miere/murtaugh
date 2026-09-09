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
// # Only the elected gateway accepts, and a standby says where to go
//
// That sentence was prose here before anything enforced it, and a standby
// happily accepted nodes it could never serve. It is now a predicate on the
// accept path: FollowLeader installs the election, an un-wired Host accepts
// nothing, and the check is the election's VERIFYING one rather than its cached
// boolean — accepting a node is externally visible and long-lived, so a
// suspended standby that woke up still believing it leads must not take one.
//
// A gateway that cannot accept REDIRECTS rather than dropping the connection. A
// bare socket close is indistinguishable from a dead gateway, a rejected
// credential and broken wifi, which is a support ticket with no evidence in it;
// so the refusal is an HTTP status the node can act on, carrying the leader's
// address in a header. The standby knows that address for free: it is already
// contending for the election lock, and the leader writes where it accepts nodes
// onto the lock record on promotion.
//
// Three answers, because a node has three different things to do about them:
// 421 naming the leader (hop, at once), 421 naming nobody (the leader accepts no
// nodes — there is nowhere in the fleet to go), and 503 (no gateway is elected
// yet — wait). The credential is checked FIRST, so none of it is available to a
// caller that has not proved which node it is.
//
// Demotion drops every attached node. A node cannot discover on its own that
// the gateway it holds a socket to has stopped leading, and left attached it
// would hold a connection nothing will ever route a conversation over.
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
// # Delegation: which node takes a conversation
//
// The choice is #170's algorithm and nothing else. Ask which nodes in the FLEET
// claim this DM or channel; exactly one takes it; more than one round robins
// among the matching; none round robins among the whole fleet. Each node
// evaluates its own ordered rule list, first match wins, yes or no. The gateway
// never merges rule lists, so there is no specificity ordering and no tie-break
// to get wrong, and there is no default node — step four already catches every
// unclaimed conversation.
//
// The FLEET is the initiating user's own connected nodes, or — only when they
// have none — the connected nodes they hold a grant on. Never a mixture, which
// is what makes user choice and node claims incapable of conflicting.
//
// The choice is then PINNED, and the pin is stored: a fourth side store beside
// the leader lock, the run claim and the node credential, keyed by the
// conversation. When the pinned node is gone the pin is OVERWRITTEN rather than
// bypassed, or the conversation re-elects on every turn and lands somewhere new
// each time. See delegate.go for the whole of it, and takeover.go for what the
// model is told when its conversation arrives from a machine that is gone.
//
// An election happens once per SESSION, not once per turn, because the session
// it opens is bound to the connection that minted it and every later prompt,
// cancel and close for that session goes back there. That binding is also what
// stops a node attaching mid-conversation from taking the conversation over:
// session ids are minted per node, and a newcomer handed an id it never issued
// fails the prompt in front of the user and answers the cancel as success. A
// session whose connection has gone is agent.ErrSessionGone, which is what
// starts the re-election.
//
// Delegation sits UNDER *agent.SessionManager, at agent.Client, because the
// gateway type-asserts optional capability surfaces on the manager and three of
// them fail silently when unsatisfied. That placement costs one thing and it is
// paid on the context: NewSession is handed metadata with no DM flag and Prompt
// is handed only a session id, so the conversation key travels as a context
// value the manager sets.
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
// journal, on the gateway stream, as kind `node`; a conversation moving between
// nodes goes there as kind `delegation`. #170 is explicit that
// disconnects are journalled and not announced, because a laptop sleeping at
// six o'clock disconnects every evening and a nightly message trains the admin
// to ignore the one that matters.
package nodehost
