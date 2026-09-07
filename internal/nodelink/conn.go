package nodelink

import (
	"errors"
	"io"
	"net"
	"sync"
)

// Conn is the message-oriented byte transport a Link runs over: one call, one
// frame, in order, no partial reads.
//
// It is this small so the transport can be replaced without touching a line of
// the delivery logic. The stage that puts a node on a socket supplies a
// WebSocket implementation (gorilla/websocket is already in go.mod, pulled in
// by slack-go's socketmode, and needs only promoting to a direct dependency);
// this package ships the in-memory duplex pair below and nothing that dials or
// listens.
type Conn interface {
	// ReadMessage returns the next whole frame, or an error once the transport
	// is done. It is called from one goroutine only — the Link's read loop.
	ReadMessage() ([]byte, error)
	// WriteMessage sends one whole frame. The Link serialises its own calls;
	// an implementation need not be safe for concurrent use.
	//
	// It must bound its own wait — a WebSocket sets a write deadline. The Link
	// cannot bound it: a write is the one place ordering forbids walking away,
	// because the next frame's sequence number has already been issued. A
	// transport that can block forever wedges the sender for as long as it
	// does, whatever context the caller supplied.
	WriteMessage([]byte) error
	// Close releases the transport. It must unblock a pending ReadMessage.
	Close() error
}

// Pipe returns the two ends of an in-memory duplex transport, with buffer
// frames of slack in each direction. A buffer of zero makes every write block
// until the peer reads it, which is how a test poses "the connection is wedged
// mid-write".
func Pipe(buffer int) (Conn, Conn) {
	left := make(chan []byte, buffer)
	right := make(chan []byte, buffer)
	a := &pipeConn{in: left, out: right, mine: make(chan struct{})}
	b := &pipeConn{in: right, out: left, mine: make(chan struct{})}
	a.theirs, b.theirs = b.mine, a.mine
	return a, b
}

type pipeConn struct {
	in   <-chan []byte
	out  chan<- []byte
	mine chan struct{}
	// theirs is the peer's close signal, so a read reports EOF when the far end
	// hangs up rather than blocking forever.
	theirs <-chan struct{}
	once   sync.Once
}

func (c *pipeConn) ReadMessage() ([]byte, error) {
	// Anything already buffered outranks a close: a peer that wrote and then
	// hung up still delivered what it wrote.
	select {
	case raw := <-c.in:
		return raw, nil
	default:
	}
	select {
	case raw := <-c.in:
		return raw, nil
	case <-c.mine:
		return nil, net.ErrClosed
	case <-c.theirs:
		select {
		case raw := <-c.in:
			return raw, nil
		default:
			return nil, io.EOF
		}
	}
}

func (c *pipeConn) WriteMessage(raw []byte) error {
	select {
	case <-c.mine:
		return net.ErrClosed
	case <-c.theirs:
		return io.ErrClosedPipe
	default:
	}
	select {
	case c.out <- raw:
		return nil
	case <-c.mine:
		return net.ErrClosed
	case <-c.theirs:
		return io.ErrClosedPipe
	}
}

func (c *pipeConn) Close() error {
	c.once.Do(func() { close(c.mine) })
	return nil
}

// isClosedConn reports whether err is the transport reporting an orderly
// shutdown rather than a fault. Both ends of Pipe and a closed socket produce
// one of these, and a link that was deliberately closed should not report a
// failure it did not have.
func isClosedConn(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
}
