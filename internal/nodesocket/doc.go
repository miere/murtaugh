// Package nodesocket is the WebSocket transport a runtime node holds open to a
// gateway: a nodelink.Conn over gorilla/websocket, one dialler, one upgrader.
//
// # Direction
//
// The node dials, the gateway answers, and there is deliberately no function
// here that lets a gateway dial a node. That is #170's attack-surface argument
// and it is a property of this package's API rather than a convention: a
// gateway holding an inbound-only endpoint cannot be turned into a client of
// every laptop in the fleet by a later refactor.
//
// # Why the write deadline is not defensive
//
// nodelink.Conn's contract says WriteMessage must bound its own wait. Measured
// against gorilla v1.5.3 on macOS loopback while building this: a peer that
// upgrades and then never reads stalls the writer after 524-557 KB of wire
// bytes, at every payload size, matching SO_SNDBUF+SO_RCVBUF (146,988+408,192 =
// 555,180) to within one frame. WriteMessage writes straight through to the
// net.Conn on the caller's goroutine — there is no library queue and no
// goroutine to absorb it. So a write to a peer that has stopped reading blocks
// forever unless something bounds it.
//
// Two consequences shape the code below.
//
// The deadline must be set immediately BEFORE each write. gorilla's
// SetWriteDeadline only stores a field that is applied at write entry, so
// setting it from a watchdog goroutine does nothing to a write already in
// flight (measured: still stuck after five seconds) and races the writer.
//
// A write that does time out is fatal to the connection, not to the frame.
// gorilla latches writeErr on any write error and every later write on that
// conn returns it (measured). There is no way to retry a frame, and pretending
// otherwise would leave a Link that believes it is delivering. So a write error
// closes the socket, which unblocks the read loop, which fails the Link — one
// teardown, one reconnect, no half-dead connection.
//
// # Backpressure lives above this
//
// Because the transport's own bound is two kernel socket buffers (~555 KB
// measured), a Link running the 4 MiB default window would fill the socket
// first and block in Link.write — which takes no context — instead of in
// awaitRoom, which honours one. DefaultWindowBytes is therefore set below the
// measured socket capacity, and the callers here pass it. Raising it without
// raising SO_SNDBUF/SO_RCVBUF moves the block back into the uncancellable path.
package nodesocket
