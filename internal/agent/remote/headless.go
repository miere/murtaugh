package remote

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentwire"
)

const maxHeadlessSignIns = 4

var headlessSignInDeadline = 20 * time.Minute

type headlessSignIn struct {
	key     string
	settled chan agent.SignInSettled
	drawn   chan struct{}
}

func (c *Client) serveSignIn(msg agentwire.Message) {
	var req agentwire.SignInRequest
	if err := msg.Into(&req); err != nil {
		c.answer(msg.ID, nil, err)
		return
	}
	switch {
	case c.owner == "":
		c.log.Warn("remote: refusing a sign-in from a node whose token names no owner", "id", req.ID)
		c.answer(msg.ID, nil, errors.New("this machine's token names no owner, so nobody can be asked to sign in"))
		return
	case c.signIn == nil:
		c.answer(msg.ID, nil, errors.New("this gateway cannot show a sign-in outside a conversation"))
		return
	case req.ID == "":
		c.answer(msg.ID, nil, errors.New("this sign-in carries no id"))
		return
	}
	open := &headlessSignIn{key: req.Tool + "\x00" + req.Profile, settled: make(chan agent.SignInSettled, 4), drawn: make(chan struct{})}
	if err := c.admitHeadless(req.ID, open); err != nil {
		c.log.Warn("remote: refusing a sign-in with no conversation", "id", req.ID, "tool", req.Tool, "error", err)
		c.answer(msg.ID, nil, err)
		return
	}
	ev, _, err := c.decoder.Decode(context.Background(), agentwire.Event{Type: agentwire.EventSignIn, SignIn: &req})
	if err != nil {
		c.mu.Lock()
		delete(c.headless, req.ID)
		c.mu.Unlock()
		c.answer(msg.ID, nil, err)
		return
	}
	prompt := ev.SignIn
	prompt.Owner = c.owner

	ctx, cancel := context.WithTimeout(context.Background(), headlessSignInDeadline)
	var once sync.Once
	shown := func(err error) {
		once.Do(func() {
			if err != nil {
				c.answer(msg.ID, nil, err)
				return
			}
			c.answer(msg.ID, agentwire.Empty{}, nil)
		})
	}
	go func() {
		defer close(open.drawn)
		c.signIn(ctx, prompt, open.settled, shown)
		shown(errors.New("the sign-in ended before it could be shown"))
	}()
	go c.relayHeadlessSignIn(req.ID, prompt, open, cancel)
}

func (c *Client) admitHeadless(id string, open *headlessSignIn) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, taken := c.headless[id]; taken || c.decoder.SignInOpen(id) {
		return errors.New("a sign-in with this id is already open")
	}
	if len(c.headless) >= maxHeadlessSignIns {
		return fmt.Errorf("this machine already has %d sign-ins waiting on its owner", len(c.headless))
	}
	for _, other := range c.headless {
		if other.key == open.key {
			return errors.New("this machine already has this sign-in waiting on its owner")
		}
	}
	c.headless[id] = open
	return nil
}

func (c *Client) relayHeadlessSignIn(id string, prompt *agent.SignInPrompt, open *headlessSignIn, cancel context.CancelFunc) {
	defer func() {
		cancel()
		c.mu.Lock()
		delete(c.headless, id)
		c.mu.Unlock()
		c.decoder.ForgetSignIn(id)
	}()
	for {
		select {
		case answer := <-prompt.Answer:
			c.sendDisplayAnswer(agentwire.EncodeDisplayAnswer(id, answer))
		case <-open.drawn:
			select {
			case answer := <-prompt.Answer:
				c.sendDisplayAnswer(agentwire.EncodeDisplayAnswer(id, answer))
			default:
			}
			return
		case <-c.link.Done():
			cancel()
			<-open.drawn
			return
		}
	}
}

func (c *Client) serveSignInSettled(msg agentwire.Message) {
	var update agentwire.SignInSettled
	if err := msg.Into(&update); err != nil {
		c.answer(msg.ID, nil, err)
		return
	}
	c.mu.Lock()
	open := c.headless[update.ID]
	c.mu.Unlock()
	if open == nil || !c.decoder.SignInOpen(update.ID) {
		c.log.Debug("remote: a sign-in settled that nothing here is drawing", "id", update.ID, "state", update.State)
		c.answer(msg.ID, agentwire.Empty{}, nil)
		return
	}
	ev, _, err := c.decoder.Decode(context.Background(), agentwire.Event{Type: agentwire.EventSignInSettled, SignInSettled: &update})
	if err != nil {
		c.answer(msg.ID, nil, err)
		return
	}
	select {
	case open.settled <- *ev.SignInSettled:
	case <-open.drawn:
	case <-c.link.Done():
	}
	c.answer(msg.ID, agentwire.Empty{}, nil)
}
