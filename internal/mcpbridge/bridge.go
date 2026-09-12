// Package mcpbridge pipes an agent's stdio to a socket the runtime owns, so
// third-party MCP credentials and the approval broker never enter its process.
package mcpbridge

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/miere/murtaugh/internal/frontends/mcp"
	"github.com/miere/murtaugh/internal/tools"
)

// Subcommand is the argv[1] that runs the bridge: `murtaugh-runtime mcp-bridge`.
const Subcommand = "mcp-bridge"

// Both sides reference EnvSocket and EnvToken, so the contract between the
// runtime and the bridge subcommand stays in one place.
const (
	EnvSocket = "MURTAUGH_BRIDGE_SOCKET"
	EnvToken  = "MURTAUGH_BRIDGE_TOKEN"
)

// handshake is the single newline-delimited JSON line the bridge sends before
// any MCP traffic. It authenticates the connection to a registered session.
type handshake struct {
	Token string `json:"token"`
}

// Session is what a registered ACP session exposes through the aggregator.
type Session struct {
	// Tools is the resolved per-agent toolset to serve (toolset.Resolve output).
	Tools []tools.Tool
	// Approver gates tool calls; nil means ungated.
	Approver mcp.Approver
	// WithContext, when set, decorates the context each tool Invoke runs under —
	// used to carry the session's Slack TurnLocation so the approver posts to the
	// right thread. nil is identity.
	WithContext func(context.Context) context.Context
	// Aliases are the names this session's backend knows some tools by; nil publishes
	// every tool under its own name.
	Aliases map[string]string
}

// Server is the runtime-side aggregator: a unix-socket listener that serves each
// registered session's toolset as an MCP server.
type Server struct {
	socketPath string
	log        *slog.Logger

	mu       sync.Mutex
	sessions map[string]Session
	listener net.Listener
	run      uint64
}

// NewServer creates a Server that will listen on socketPath. The socket's parent
// directory is created with 0700 and the socket itself with 0600 on Start, so
// only Murtaugh's own user can connect — defence in depth alongside the token.
func NewServer(socketPath string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{socketPath: socketPath, log: log, sessions: make(map[string]Session)}
}

// Start binds the socket and runs the accept loop until ctx is cancelled or
// Close is called. It returns once the listener stops.
func (s *Server) Start(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0o700); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}

	s.mu.Lock()
	if prev := s.listener; prev != nil {
		_ = prev.Close()
	}
	// A stale socket from a previous run would make Listen fail with "address
	// already in use"; remove it first. It is in our private 0700 dir.
	_ = os.Remove(s.socketPath)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.socketPath, Net: "unix"})
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("listen on %s: %w", s.socketPath, err)
	}
	ln.SetUnlinkOnClose(false)
	if err := os.Chmod(s.socketPath, 0o600); err != nil {
		_ = ln.Close()
		s.mu.Unlock()
		return fmt.Errorf("chmod socket: %w", err)
	}
	s.run++
	run := s.run
	s.listener = ln
	s.mu.Unlock()

	go func() {
		<-ctx.Done()
		s.stop(run, ln)
	}()

	s.log.Info("mcp aggregator listening", "socket", s.socketPath, "run", run)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.log.Warn("mcp aggregator accept failed", "error", err)
			continue
		}
		go s.serveConn(ctx, conn)
	}
}

func (s *Server) stop(run uint64, ln net.Listener) error {
	s.mu.Lock()
	if run == s.run {
		s.listener = nil
	}
	s.mu.Unlock()

	err := ln.Close()

	s.mu.Lock()
	if run == s.run {
		_ = os.Remove(s.socketPath)
	}
	s.mu.Unlock()
	return err
}

// Register binds a resolved toolset to a fresh token and returns it. The caller
// passes the token to the bridge (via env) so the agent's spawned bridge can
// claim this session. Unregister when the session ends.
func (s *Server) Register(sess Session) (token string, err error) {
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.sessions[tok] = sess
	s.mu.Unlock()
	return tok, nil
}

// Unregister drops a session's token so no further bridge connections can claim
// it. In-flight connections are unaffected.
func (s *Server) Unregister(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// SocketPath is where the server listens; the value to give the bridge.
func (s *Server) SocketPath() string { return s.socketPath }

// Does not retire the server: a later Start serves again, because stepping down
// and being promoted again is routine for a leader.
func (s *Server) Close() error {
	s.mu.Lock()
	ln, run := s.listener, s.run
	s.mu.Unlock()
	if ln == nil {
		return nil
	}
	return s.stop(run, ln)
}

// serveConn reads the handshake, resolves the session, and runs an MCP server
// over the connection bound to that session's toolset.
func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	// Read exactly the handshake line, keeping any buffered MCP bytes for the
	// transport by reusing the same bufio.Reader below.
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil {
		s.log.Warn("mcp aggregator handshake read failed", "error", err)
		_ = conn.Close()
		return
	}
	var hs handshake
	if err := json.Unmarshal(line, &hs); err != nil {
		s.log.Warn("mcp aggregator handshake decode failed", "error", err)
		_ = conn.Close()
		return
	}
	s.mu.Lock()
	sess, ok := s.sessions[hs.Token]
	s.mu.Unlock()
	if !ok {
		// Unknown or already-unregistered token: refuse silently (no oracle).
		s.log.Warn("mcp aggregator rejected unknown token")
		_ = conn.Close()
		return
	}

	runCtx := ctx
	if sess.WithContext != nil {
		runCtx = sess.WithContext(runCtx)
	}
	transport := &mcpsdk.IOTransport{
		Reader: connReader{r: br, c: conn},
		Writer: conn,
	}
	server := mcp.NewFromTools(sess.Tools, sess.Approver, sess.Aliases).Server()
	s.logSessionEnd(server.Run(runCtx, transport))
}

// Every new conversation is tried as a resume first, and the process torn down
// when that fails takes its bridge with it — a teardown, not a fault.
func (s *Server) logSessionEnd(err error) {
	switch {
	case err == nil:
	case errors.Is(err, io.EOF), errors.Is(err, net.ErrClosed), errors.Is(err, context.Canceled):
		s.log.Debug("mcp aggregator session ended", "error", err)
	default:
		s.log.Warn("mcp aggregator session ended unexpectedly", "error", err)
	}
}

// connReader adapts a buffered reader plus the underlying closer into the
// io.ReadCloser the IOTransport wants, so handshake-buffered bytes are not lost.
type connReader struct {
	r io.Reader
	c io.Closer
}

func (cr connReader) Read(p []byte) (int, error) { return cr.r.Read(p) }
func (cr connReader) Close() error               { return cr.c.Close() }

// RunBridge speaks no MCP itself — it is a transparent pipe, so the runtime's
// MCP server and the agent's MCP client talk directly.
func RunBridge(ctx context.Context, socketPath, token string, in io.Reader, out io.Writer) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("dial aggregator socket: %w", err)
	}
	defer func() { _ = conn.Close() }()

	line, err := json.Marshal(handshake{Token: token})
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("send handshake: %w", err)
	}

	// Close the connection when ctx is cancelled so the copies unblock.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	errc := make(chan error, 2)
	go func() { _, err := io.Copy(conn, in); errc <- err }()  // agent -> runtime
	go func() { _, err := io.Copy(out, conn); errc <- err }() // runtime -> agent
	// Return as soon as either direction ends; the deferred Close tears down the
	// other copy.
	err = <-errc
	// Propagate the upstream close downstream, so an MCP client reading the
	// bridge's stdout sees EOF instead of blocking forever.
	if c, ok := out.(io.Closer); ok {
		_ = c.Close()
	}
	if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// newToken returns an unguessable session token.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
