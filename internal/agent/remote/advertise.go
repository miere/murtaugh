package remote

import (
	"context"
	"errors"
	"fmt"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/nodelink"
)

// Advertiser receives what one node claims.
//
// It is called from two places and the distinction is the whole ordering
// story. The connect-time claim is delivered from Initialize, on the caller's
// goroutine, BEFORE Initialize returns — so a gateway that publishes a node
// after a successful handshake has the claim in hand by then, with no window in
// which the node is attached and mute. Every later claim is delivered from the
// link's request dispatch.
//
// Both deliveries are a full snapshot that REPLACES the previous one. An
// implementation that merged them would need an ordering guarantee across the
// two paths that neither provides.
type Advertiser interface {
	Advertise(ad agentwire.Advertisement)
}

// AdvertiserFunc adapts a function to Advertiser.
type AdvertiserFunc func(agentwire.Advertisement)

// Advertise implements Advertiser.
func (f AdvertiserFunc) Advertise(ad agentwire.Advertisement) { f(ad) }

// serveAdvertise applies a node's changed claim and acknowledges it.
//
// The answer is sent whether or not anything is bound, and it is deliberately
// not a fault when nothing is: the node did its job. A gateway with no
// advertiser is one running without a registry, which is every gateway before
// #195 and is a gateway-side choice the node cannot do anything about — failing
// its push would make its own logs blame it for the gateway's shape.
func (c *Client) serveAdvertise(msg agentwire.Message) {
	var ad agentwire.Advertisement
	if err := msg.Into(&ad); err != nil {
		c.answer(msg.ID, nil, err)
		return
	}
	c.applyAdvertisement(ad, false)
	c.answer(msg.ID, agentwire.Empty{}, nil)
}

// applyAdvertisement records the claim and hands it on.
//
// opening marks the claim that rode the handshake answer, and the guard it
// takes is the one ordering rule this whole feature has. The two paths converge
// here from DIFFERENT goroutines — the opening claim from whoever called
// Initialize, a pushed change from the link's request dispatch — so a node that
// edits its configuration in the shadow of its own handshake can have the
// change applied before the answer that preceded it on the wire is read back.
// Wire order is preserved; goroutine order is not. An opening claim therefore
// yields to anything already recorded, and the newest claim wins whichever path
// it came in on. The rule lives here rather than in the registry because this is
// the only place that can tell the two apart.
//
// The client keeps no copy of the claim, only the flag that says one has
// landed. The registry holds the claim, and a second copy here would be a
// second answer to "what does this node serve" that nothing reconciles. The
// clone is therefore not tidiness either: the value handed over is held for the
// life of the connection and read from another goroutine, so it must not share
// a slice header with the caller's.
func (c *Client) applyAdvertisement(ad agentwire.Advertisement, opening bool) {
	c.mu.Lock()
	if opening && c.advertisedOnce {
		c.mu.Unlock()
		return
	}
	c.advertisedOnce = true
	advertiser := c.advertiser
	c.mu.Unlock()
	if advertiser == nil {
		return
	}
	advertiser.Advertise(ad.Clone())
}

func (c *Client) serveRequest(msg agentwire.Message) {
	switch msg.Method {
	case agentwire.MethodAdvertise:
		c.serveAdvertise(msg)
	case agentwire.MethodSignIn:
		c.serveSignIn(msg)
	case agentwire.MethodSignInSettled:
		c.serveSignInSettled(msg)
	default:
		c.rejectRequest(msg)
	}
}

func (c *Client) answer(id string, body any, failure error) {
	var msg agentwire.Message
	if failure != nil {
		msg = agentwire.Fault(id, failure)
	} else {
		encoded, err := agentwire.Result(id, body)
		if err != nil {
			msg = agentwire.Fault(id, fmt.Errorf("remote: encode answer: %w", err))
		} else {
			msg = encoded
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), answerTimeout)
	defer cancel()
	if err := c.send(ctx, msg); err != nil && !errors.Is(err, nodelink.ErrLinkClosed) {
		c.log.Warn("remote: deliver answer to the node", "error", err, "id", id)
	}
}
