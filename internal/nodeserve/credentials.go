package nodeserve

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/nodelink"
)

type Credentials struct {
	log     *slog.Logger
	current func() []agentwire.CredentialHealth

	mu      sync.Mutex
	server  *Server
	sending sync.Mutex
}

func NewCredentials(log *slog.Logger, current func() []agentwire.CredentialHealth) *Credentials {
	if log == nil {
		log = slog.Default()
	}
	return &Credentials{log: log, current: current}
}

func (c *Credentials) bind(s *Server) {
	c.mu.Lock()
	c.server = s
	c.mu.Unlock()
	if c.current == nil {
		return
	}
	go func() {
		c.sending.Lock()
		defer c.sending.Unlock()
		for _, h := range c.current() {
			c.push(s, h)
		}
	}()
}

func (c *Credentials) unbind(s *Server) {
	c.mu.Lock()
	if c.server == s {
		c.server = nil
	}
	c.mu.Unlock()
}

func (c *Credentials) Report(h agentwire.CredentialHealth) {
	c.mu.Lock()
	server := c.server
	c.mu.Unlock()
	if server == nil {
		c.log.Debug("nodeserve: holding a credential report; no gateway is attached", "credential", h.Credential, "degraded", h.Degraded)
		return
	}
	c.sending.Lock()
	defer c.sending.Unlock()
	c.push(server, h)
}

func (c *Credentials) push(s *Server, h agentwire.CredentialHealth) {
	ctx, cancel := context.WithTimeout(s.ctx, advertiseTimeout)
	defer cancel()
	_, err := s.callGateway(ctx, agentwire.MethodCredentialHealth, h)
	if err != nil && !errors.Is(err, errLinkGone) && !errors.Is(err, nodelink.ErrLinkClosed) && !errors.Is(err, context.Canceled) {
		c.log.Warn("nodeserve: could not tell the gateway how a credential is doing", "credential", h.Credential, "error", err)
	}
}
