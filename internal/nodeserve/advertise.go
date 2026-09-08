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

// advertiseTimeout bounds one push. It is short because an advertisement is a
// small frame carrying a fact that is already stale if it takes a minute to
// land, and because the next connect re-sends the whole claim anyway.
const advertiseTimeout = 30 * time.Second

// Advertiser is what this node tells a gateway it can serve: the agent profile
// names it actually serves, and the channels it claims an assignment rule for.
//
// It has the same shape as BackgroundSink and ToolGate, and for the same
// reason: it is built at startup, before any connection exists, and bound to
// whichever server is currently serving. What is different is that it also
// HOLDS the claim, because the claim outlives any one connection — the node's
// configuration does not change just because its socket did.
//
// Unbound it DROPS, and dropping is correct rather than a compromise. The value
// is kept, and a fresh connection carries the whole claim on its handshake
// answer, so a push made while nothing is attached would be a duplicate of what
// the next connect sends. Queueing would mean replaying a stale claim at the
// exact moment a current one is already on its way.
type Advertiser struct {
	log *slog.Logger

	mu      sync.Mutex
	current agentwire.Advertisement
	server  *Server
}

// NewAdvertiser returns an unbound advertiser claiming nothing.
//
// Claiming nothing is a real state and not a placeholder: a node whose
// configuration defines no agents has never been set up, and #170 makes that
// the trigger to onboard its owner rather than an error to report.
func NewAdvertiser(log *slog.Logger) *Advertiser {
	if log == nil {
		log = slog.Default()
	}
	return &Advertiser{log: log}
}

// Publish replaces what this node claims and pushes it to the attached gateway.
//
// It is a FULL SNAPSHOT every time. There is no resume across a reconnect and
// the link drops duplicates silently, so a delta would need ordering guarantees
// against the connect-time state that nothing provides; replacing the whole
// claim set makes a re-sent or reordered advertisement harmless, which is what
// lets the caller push whenever it is unsure rather than tracking what the
// gateway last heard.
//
// It must not be called from the link's read loop: it waits for the gateway's
// answer, and a frame is acknowledged only once its handler returns.
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
		// Not fatal and not retried. The connection that failed is on its way
		// down, and the redial that follows carries this exact value on its
		// handshake — a retry loop here would race that and win nothing.
		if !errors.Is(err, nodelink.ErrLinkClosed) && !errors.Is(err, context.Canceled) {
			a.log.Warn("nodeserve: could not tell the gateway what this node claims", "error", err)
		}
		return
	}
	a.log.Info("told the gateway what this node claims", "profiles", len(ad.Profiles), "claims", len(ad.Claims))
}

// Current is what this node claims right now. The handshake answer carries it.
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

// advertise sends one changed claim and waits for the gateway to acknowledge it.
//
// The acknowledgement is why this is a request and not a one-way event: a node
// whose claim was rejected and which could not tell would go on believing it
// serves channels the gateway will never send it, and the only symptom would be
// conversations quietly landing elsewhere.
func (s *Server) advertise(ctx context.Context, ad agentwire.Advertisement) error {
	ctx, cancel := context.WithTimeout(ctx, advertiseTimeout)
	defer cancel()
	_, err := s.callGateway(ctx, agentwire.MethodAdvertise, ad)
	return err
}
