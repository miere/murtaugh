package nodeserve

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/nodelink"
)

const advertiseTimeout = 30 * time.Second

// Unbound, it drops pushes but keeps the value: the next connection sends the whole claim in its
// handshake, so a queued push would replay a stale claim over a current one.
type Advertiser struct {
	log *slog.Logger

	mu      sync.Mutex
	current agentwire.Advertisement
	server  *Server
}

// Claiming nothing is a real state, not a placeholder: a node with no agents is what triggers
// onboarding its owner.
func NewAdvertiser(log *slog.Logger) *Advertiser {
	if log == nil {
		log = slog.Default()
	}
	return &Advertiser{log: log}
}

// Sends a full snapshot so a re-sent or reordered push is harmless. Never call it from the link's
// read loop: it waits for the gateway's answer, which that loop has to deliver.
func (a *Advertiser) Publish(ctx context.Context, ad agentwire.Advertisement) {
	a.mu.Lock()
	a.current = ad.Clone()
	server := a.server
	a.mu.Unlock()
	if server == nil {
		a.log.Debug("nodeserve: holding this node's claim; no gateway is attached", "profiles", len(ad.Profiles), "claims", len(ad.Claims))
		return
	}
	if err := server.advertise(ctx, ad); err != nil {
		if !errors.Is(err, nodelink.ErrLinkClosed) && !errors.Is(err, context.Canceled) {
			a.log.Warn("nodeserve: could not tell the gateway what this node claims", "error", err)
		}
		return
	}
	a.log.Info("told the gateway what this node claims", "profiles", len(ad.Profiles), "claims", len(ad.Claims))
}

func (a *Advertiser) Current() agentwire.Advertisement {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.current.Clone()
}

func (a *Advertiser) bind(s *Server) {
	a.mu.Lock()
	a.server = s
	a.mu.Unlock()
}

func (a *Advertiser) unbind(s *Server) {
	a.mu.Lock()
	if a.server == s {
		a.server = nil
	}
	a.mu.Unlock()
}

func (s *Server) advertise(ctx context.Context, ad agentwire.Advertisement) error {
	ctx, cancel := context.WithTimeout(ctx, advertiseTimeout)
	defer cancel()
	_, err := s.callGateway(ctx, agentwire.MethodAdvertise, ad)
	return err
}
