package remote

import (
	"context"
	"errors"
	"fmt"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/nodelink"
)

// Advertiser gets a full snapshot each time, and it must replace the previous one: the
// connect-time and later claims arrive on different paths with no ordering between them.
type Advertiser interface {
	Advertise(ad agentwire.Advertisement)
}

type AdvertiserFunc func(agentwire.Advertisement)

func (f AdvertiserFunc) Advertise(ad agentwire.Advertisement) { f(ad) }

func (c *Client) serveAdvertise(msg agentwire.Message) {
	var ad agentwire.Advertisement
	if err := msg.Into(&ad); err != nil {
		c.answer(msg.ID, nil, err)
		return
	}
	c.applyAdvertisement(ad, false)
	c.answer(msg.ID, agentwire.Empty{}, nil)
}

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
	case agentwire.MethodCredentialHealth:
		c.serveCredentialHealth(msg)
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

func (c *Client) serveCredentialHealth(msg agentwire.Message) {
	var report agentwire.CredentialHealth
	if err := msg.Into(&report); err != nil {
		c.answer(msg.ID, nil, err)
		return
	}
	if c.credentials != nil {
		c.credentials(report)
	}
	c.answer(msg.ID, agentwire.Empty{}, nil)
}
