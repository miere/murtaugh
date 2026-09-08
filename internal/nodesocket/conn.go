package nodesocket

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// Path is the endpoint a node dials on the gateway. It is a constant rather
	// than configuration because both ends ship from this repository: an
	// operator who could get it wrong gains nothing by being allowed to.
	Path = "/murtaugh/node/link"

	// bufferSize is the read and write buffer on both ends. gorilla defaults to
	// 4 KiB, which fragments anything larger into continuation frames —
	// slack-go had to raise its own socket-mode buffers to 32 KiB for exactly
	// that reason. Our largest ordinary frame is a 4 MiB attachment chunk, so
	// the default would cost a thousand fragments per chunk.
	bufferSize = 32 << 10

	// DefaultWriteTimeout bounds one WriteMessage. It is generous because it is
	// a link-teardown trigger and not a latency budget: exceeding it means the
	// peer has not drained a socket buffer in half a minute, which is a dead
	// connection rather than a slow one.
	DefaultWriteTimeout = 30 * time.Second

	// DefaultWindowBytes is the unacknowledged window to run a Link with over
	// this transport. It is deliberately BELOW the measured socket-buffer
	// capacity (~555 KB on macOS loopback) so a peer that stops consuming parks
	// the sender in nodelink's awaitRoom, which honours a context, rather than
	// in the transport write, which cannot.
	DefaultWindowBytes = 256 << 10

	// handshakeTimeout bounds the HTTP upgrade.
	handshakeTimeout = 15 * time.Second
)

// Conn is a nodelink.Conn over one WebSocket.
//
// It is not safe for concurrent writes and does not try to be: gorilla panics
// on a second concurrent writer rather than returning an error, and nodelink
// already funnels every write through one mutex. Adding a lock here would hide
// a caller that violates that rule instead of surfacing it.
type Conn struct {
	ws      *websocket.Conn
	timeout time.Duration

	mu   sync.Mutex
	dead error
}

func newConn(ws *websocket.Conn, timeout time.Duration) *Conn {
	if timeout <= 0 {
		timeout = DefaultWriteTimeout
	}
	ws.SetReadLimit(0)
	return &Conn{ws: ws, timeout: timeout}
}

// ReadMessage returns the next whole frame.
//
// A peer's orderly close is reported as io.EOF, because that is what
// nodelink.isClosedConn recognises as "the far end hung up" rather than as a
// fault — a node shutting down cleanly must not be journalled as a failure.
func (c *Conn) ReadMessage() ([]byte, error) {
	_, data, err := c.ws.ReadMessage()
	if err != nil {
		return nil, translateRead(err)
	}
	return data, nil
}

// WriteMessage sends one frame under a deadline set immediately before it.
//
// Any failure — a timeout included — closes the socket. gorilla latches the
// first write error and fails every later write with it, so a connection that
// has failed one write is finished; closing here is what unblocks the read loop
// so the Link fails once, promptly, instead of hanging until something else
// notices.
//
// That deadline is also the whole answer to the wedged writer. A write already
// in flight cannot be rescued: gorilla's SetWriteDeadline does not reach it
// (measured), so nothing here can break one sender out and let the rest carry
// on. What happens instead is that the deadline set above expires, the write
// fails, and the LINK is torn down — the peer has not drained a socket buffer
// in half a minute, which is a dead connection and not a slow one. A caller
// waiting behind that write therefore waits at most one write timeout, not
// forever, and it waits for a link that is about to end.
func (c *Conn) WriteMessage(raw []byte) error {
	if err := c.err(); err != nil {
		return err
	}
	if err := c.ws.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return c.poison(err)
	}
	if err := c.ws.WriteMessage(websocket.BinaryMessage, raw); err != nil {
		return c.poison(err)
	}
	return nil
}

// Close releases the socket and unblocks a pending ReadMessage.
func (c *Conn) Close() error { return c.ws.Close() }

func (c *Conn) err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dead
}

func (c *Conn) poison(err error) error {
	c.mu.Lock()
	if c.dead == nil {
		c.dead = err
	}
	c.mu.Unlock()
	// Close is one of the two gorilla methods documented safe to call
	// concurrently with anything, which is what makes this legal from inside a
	// write.
	_ = c.ws.Close()
	return err
}

func translateRead(err error) error {
	if websocket.IsCloseError(err,
		websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseNoStatusReceived,
	) {
		return io.EOF
	}
	if errors.Is(err, net.ErrClosed) {
		return net.ErrClosed
	}
	return err
}
