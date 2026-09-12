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
	// A constant, not configuration: both ends ship from this repo, so letting an operator change it
	// only lets them get it wrong.
	Path = "/murtaugh/node/link"

	bufferSize = 32 << 10

	// Generous because it is a teardown trigger, not a latency budget: a peer that has not drained a
	// socket buffer in half a minute is dead, not slow.
	DefaultWriteTimeout = 30 * time.Second

	// Below the measured socket buffer capacity (~555 KB on macOS loopback), so a stalled peer parks the
	// sender in awaitRoom, which honours a context, rather than in the write, which cannot.
	DefaultWindowBytes = 256 << 10

	handshakeTimeout = 15 * time.Second
	closeGrace       = time.Second
)

// Not safe for concurrent writes on purpose: nodelink already serialises them and gorilla panics on
// a second writer, so a lock here would only hide a caller that breaks that rule.
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

// A peer's orderly close is reported as io.EOF so a node shutting down cleanly is not logged as a
// failure.
func (c *Conn) ReadMessage() ([]byte, error) {
	_, data, err := c.ws.ReadMessage()
	if err != nil {
		return nil, translateRead(err)
	}
	return data, nil
}

// Any failure closes the socket: gorilla fails every write after the first error, and closing
// unblocks the read loop so the Link fails once, promptly, instead of hanging.
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

// Close says goodbye first so the peer reads an orderly close, not an abnormal one it would log as a
// failure; the grace is short because a peer too wedged to take one frame gets nothing from waiting.
func (c *Conn) Close() error {
	if c.err() == nil {
		farewell := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
		_ = c.ws.WriteControl(websocket.CloseMessage, farewell, time.Now().Add(closeGrace))
	}
	return c.ws.Close()
}

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
