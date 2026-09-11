package authcard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	slacklib "github.com/miere/murtaugh/internal/slack/client"
)

// Showing carries no process, because whoever runs the sign-in may be on another
// machine and reports back through the Card.
type Showing struct {
	ToolName        string
	ProfileName     string
	URL             string
	NeedsCode       bool
	Requester       Destination
	RequesterUserID string
	// Recipient gets the card by DM and is the only person who may answer it;
	// empty means the gateway admin.
	Recipient string
	// Command, shown with no URL, is approved by the recipient before it runs;
	// its link arrives later through Link.
	Command string
}

// ErrNotAllowed lets a caller tell a recipient who may not use the gateway
// apart from Slack failing, which is not the node's business.
var ErrNotAllowed = errors.New("authcard: the recipient may not use this gateway")

// RefusedReason is fixed text because it is handed to whoever runs the sign-in,
// which may be another machine's model.
const RefusedReason = "the person asked to sign in lost access to this gateway, so the sign-in was stopped"

type ReplyKind string

const (
	ReplyCode     ReplyKind = "code"
	ReplyDenied   ReplyKind = "denied"
	ReplyApproved ReplyKind = "approved"
	// ReplyRefused means the recipient lost access while the card was open, so
	// the sign-in has to stop even though nobody declined it.
	ReplyRefused ReplyKind = "refused"
)

type Reply struct {
	Kind   ReplyKind
	Code   string
	UserID string
	Reason string
}

// Card is a drawn sign-in. It stays answerable until Settle, so a click that
// lands the moment the card is delivered is never lost.
type Card struct {
	flow      *Flow
	api       slacklib.SlackAPI
	corr      string
	showing   Showing
	recipient string
	attemptAt string
	replies   chan Reply

	mu         sync.Mutex
	url        string
	reqChannel string
	reqTS      string
	dmChannel  string
	dmTS       string
	working    bool
	approving  bool
	settled    bool
}

// Show refuses a recipient who may not use this gateway, because a card they
// could never answer would hold the sign-in open until it timed out.
func (f *Flow) Show(ctx context.Context, s Showing) (*Card, error) {
	card, err := f.open(ctx, s)
	if card == nil {
		return nil, err
	}
	if err != nil {
		card.Settle(StateFailed, err.Error())
		return nil, err
	}
	if err := card.post(ctx, s.URL); err != nil {
		card.Settle(StateFailed, err.Error())
		return nil, err
	}
	return card, nil
}

func (f *Flow) open(ctx context.Context, s Showing) (*Card, error) {
	recipient := strings.TrimSpace(s.Recipient)
	if recipient == "" {
		recipient = f.adminUser()
	}
	if recipient == "" {
		return nil, errors.New("authcard: no admin user is configured, so nobody can answer a sign-in request")
	}
	if !f.authorised(recipient) {
		return nil, fmt.Errorf("%w: %s", ErrNotAllowed, recipient)
	}
	api, err := f.client.Get()
	if err != nil {
		return nil, err
	}
	corr, err := newCorrelationID()
	if err != nil {
		return nil, err
	}
	card := &Card{
		flow:      f,
		api:       api,
		corr:      corr,
		showing:   s,
		recipient: recipient,
		attemptAt: f.now().Format(attemptFormat),
		replies:   make(chan Reply, 2),
	}

	if strings.TrimSpace(s.Requester.ChannelID) != "" {
		blocks, err := f.cards.render(RequesterTemplate, card.data(StatePending, "", false, false))
		if err != nil {
			return nil, err
		}
		posted, err := api.PostMessage(ctx, slacklib.PostMessageParams{
			ChannelID: s.Requester.ChannelID,
			ThreadTS:  s.Requester.ThreadTS,
			Text:      fallbackText(s.ToolName),
			Blocks:    blocks,
		})
		if err != nil {
			return nil, fmt.Errorf("authcard: post requester notice: %w", err)
		}
		card.reqChannel, card.reqTS = posted.Channel, posted.TS
	}

	dm, err := api.OpenDM(ctx, recipient)
	if err != nil {
		return card, fmt.Errorf("could not open a DM with %s: %w", recipient, err)
	}
	card.dmChannel = dm
	return card, nil
}

func (c *Card) post(ctx context.Context, url string) error {
	c.mu.Lock()
	c.url = url
	c.approving = url == "" && c.showing.Command != ""
	state, actions := StatePending, true
	if c.approving {
		state, actions = StateApproval, false
	}
	c.mu.Unlock()
	c.flow.register(c.corr, c)

	blocks, err := c.flow.cards.render(AdminTemplate, c.data(state, "", actions, false))
	if err != nil {
		return err
	}
	posted, err := c.api.PostMessage(ctx, slacklib.PostMessageParams{
		ChannelID: c.dmChannel,
		Text:      fallbackText(c.showing.ToolName),
		Blocks:    blocks,
	})
	if err != nil {
		return fmt.Errorf("could not deliver the sign-in card to %s: %w", c.recipient, err)
	}
	c.mu.Lock()
	c.dmChannel, c.dmTS = posted.Channel, posted.TS
	working, starting := c.working && !c.settled, !c.approving && c.url == "" && !c.settled
	c.mu.Unlock()
	switch {
	case working:
		c.updateDM(ctx, StateWorking, "", false)
	case starting:
		c.updateDM(ctx, StateStarting, "", false)
	}
	return nil
}

// Link exists because an approved command only has a link once it has run, so
// the card that asked for approval grows its buttons afterwards.
func (c *Card) Link(ctx context.Context, url string) {
	c.mu.Lock()
	if c.settled || c.approving || c.url != "" {
		c.mu.Unlock()
		return
	}
	c.url = url
	c.mu.Unlock()
	c.updateDM(ctx, StatePending, "", true)
}

// Replies is never closed, because a click can still be in flight when the
// card settles.
func (c *Card) Replies() <-chan Reply { return c.replies }

// Working exists because the first click spends the single attempt, and a card
// still showing its buttons invites a second.
func (c *Card) Working(ctx context.Context) {
	c.mu.Lock()
	if c.working || c.settled {
		c.mu.Unlock()
		return
	}
	c.working = true
	posted := c.dmTS != ""
	c.mu.Unlock()
	if posted {
		c.updateDM(ctx, StateWorking, "", false)
	}
}

// Settle runs on a fresh context because the one that drew the card is often
// already cancelled, and a card left showing live buttons is worse than a late one.
func (c *Card) Settle(state State, reason string) {
	c.mu.Lock()
	if c.settled {
		c.mu.Unlock()
		return
	}
	c.settled = true
	dmChannel, dmTS, reqChannel, reqTS := c.dmChannel, c.dmTS, c.reqChannel, c.reqTS
	c.mu.Unlock()
	c.flow.unregister(c.corr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if dmChannel != "" {
		data := c.data(state, reason, false, true)
		if blocks, err := c.flow.cards.render(AdminTemplate, data); err == nil {
			if dmTS == "" {
				_, _ = c.api.PostMessage(ctx, slacklib.PostMessageParams{ChannelID: dmChannel, Text: fallbackText(c.showing.ToolName), Blocks: blocks})
			} else {
				_, _ = c.api.UpdateMessage(ctx, slacklib.UpdateMessageParams{ChannelID: dmChannel, TS: dmTS, Text: fallbackText(c.showing.ToolName), Blocks: blocks})
			}
		}
	}
	if reqChannel == "" || reqTS == "" {
		return
	}
	blocks, err := c.flow.cards.render(RequesterTemplate, c.data(state, "", false, false))
	if err != nil {
		return
	}
	_, _ = c.api.UpdateMessage(ctx, slacklib.UpdateMessageParams{ChannelID: reqChannel, TS: reqTS, Text: fallbackText(c.showing.ToolName), Blocks: blocks})
}

func (c *Card) updateDM(ctx context.Context, state State, reason string, showActions bool) {
	c.mu.Lock()
	channel, ts := c.dmChannel, c.dmTS
	c.mu.Unlock()
	if channel == "" || ts == "" {
		return
	}
	blocks, err := c.flow.cards.render(AdminTemplate, c.data(state, reason, showActions, !showActions))
	if err != nil {
		return
	}
	_, _ = c.api.UpdateMessage(ctx, slacklib.UpdateMessageParams{ChannelID: channel, TS: ts, Text: fallbackText(c.showing.ToolName), Blocks: blocks})
}

func (c *Card) approve(ctx context.Context, userID string) error {
	c.mu.Lock()
	if !c.approving || c.settled {
		c.mu.Unlock()
		return errors.New("authcard: this sign-in has nothing waiting for approval")
	}
	c.approving = false
	c.mu.Unlock()
	c.updateDM(ctx, StateStarting, "", false)
	return c.reply(Reply{Kind: ReplyApproved, UserID: userID})
}

func (c *Card) linked() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.url != ""
}

func (c *Card) reply(r Reply) error {
	select {
	case c.replies <- r:
		return nil
	default:
		return errors.New("authcard: an answer to this sign-in is already being processed")
	}
}

func (c *Card) data(state State, reason string, showActions, showFooter bool) cardData {
	c.mu.Lock()
	url := c.url
	c.mu.Unlock()
	return cardData{
		Command:         c.showing.Command,
		ShowApproval:    state == StateApproval,
		ActionApprove:   ActionID(c.corr, ActionApprove),
		ToolName:        c.showing.ToolName,
		ProfileName:     c.showing.ProfileName,
		URL:             url,
		NeedsCode:       c.showing.NeedsCode,
		RequesterUserID: c.showing.RequesterUserID,
		RecipientUserID: c.recipient,
		AttemptAt:       c.attemptAt,
		State:           string(state),
		Reason:          reason,
		ShowActions:     showActions,
		ShowFooter:      showFooter,
		ShowRequester:   strings.TrimSpace(c.showing.RequesterUserID) != "",
		ActionPrimary:   ActionID(c.corr, ActionPrimary),
		ActionOpen:      ActionID(c.corr, ActionOpen),
		ActionDeny:      ActionID(c.corr, ActionDeny),
	}
}

func (f *Flow) admit(c *Card, userID string) error {
	userID = strings.TrimSpace(userID)
	if userID == "" || userID != c.recipient {
		return fmt.Errorf("authcard: user %s is not who this sign-in was sent to; ignored", userID)
	}
	if f.authorised(userID) {
		return nil
	}
	_ = c.reply(Reply{Kind: ReplyRefused, UserID: userID, Reason: RefusedReason})
	return fmt.Errorf("authcard: %s may no longer use this gateway; the sign-in was stopped", userID)
}

// HandleClick checks the clicker itself rather than trusting the router,
// because access can be withdrawn while a card is open.
func (f *Flow) HandleClick(ctx context.Context, corr string, action Action, userID, triggerID string) error {
	c, ok := f.card(corr)
	if !ok {
		return fmt.Errorf("authcard: no authentication request is waiting for %q", corr)
	}
	if err := f.admit(c, userID); err != nil {
		return err
	}
	switch action {
	case ActionDeny:
		return c.reply(Reply{Kind: ReplyDenied, UserID: userID})
	case ActionApprove:
		return c.approve(ctx, userID)
	case ActionPrimary:
		if !c.linked() {
			return errors.New("authcard: this sign-in has no link to open yet")
		}
		c.Working(ctx)
		if !c.showing.NeedsCode {
			return nil
		}
		return c.api.OpenView(ctx, triggerID, CodeModal(corr, c.showing.ToolName))
	case ActionOpen:
		return nil
	}
	return fmt.Errorf("authcard: unknown auth action %q", action)
}

// HandleCodeSubmission is held to the same check as HandleClick, for the same
// reasons.
func (f *Flow) HandleCodeSubmission(corr, code, userID string) error {
	if strings.TrimSpace(code) == "" {
		return errors.New("authcard: the verification code was empty")
	}
	c, ok := f.card(corr)
	if !ok {
		return fmt.Errorf("authcard: no authentication request is waiting for %q", corr)
	}
	if err := f.admit(c, userID); err != nil {
		return err
	}
	return c.reply(Reply{Kind: ReplyCode, Code: strings.TrimSpace(code), UserID: userID})
}
