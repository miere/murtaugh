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
