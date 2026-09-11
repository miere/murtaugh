package nodelink

import (
	"errors"
	"io"
	"net"
	"sync"
)

type Conn interface {
	ReadMessage() ([]byte, error)
	// Implementations must bound their own wait: the Link cannot give up on a write once the next
	// frame's sequence number has been issued.
	WriteMessage([]byte) error
	// Close must unblock a pending ReadMessage, or the Link's read loop never exits.
	Close() error
}

func Pipe(buffer int) (Conn, Conn) {
	left := make(chan []byte, buffer)
	right := make(chan []byte, buffer)
	a := &pipeConn{in: left, out: right, mine: make(chan struct{})}
	b := &pipeConn{in: right, out: left, mine: make(chan struct{})}
	a.theirs, b.theirs = b.mine, a.mine
	return a, b
}

type pipeConn struct {
	in     <-chan []byte
	out    chan<- []byte
	mine   chan struct{}
	theirs <-chan struct{}
	once   sync.Once
}

func (c *pipeConn) ReadMessage() ([]byte, error) {
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

func isClosedConn(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
}
