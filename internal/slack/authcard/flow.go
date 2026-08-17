package authcard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/miere/murtaugh/internal/auth"
	"github.com/miere/murtaugh/internal/proc"
	slacklib "github.com/miere/murtaugh/internal/slack/client"
)

const (
	// DefaultTimeout bounds one authentication request end to end. Long enough
	// for an admin to notice a DM, open a browser and sign in; short enough
	// that a forgotten request does not hold a turn open indefinitely.
	DefaultTimeout = 10 * time.Minute

	// DefaultURLWait bounds how long we wait for the auth command to print its
	// verification URL. A flow that has not produced one by then is broken (a
	// missing binary, an unexpected prompt) and there is nothing to show the
	// admin, so it fails rather than posting a card with no link.
	DefaultURLWait = 60 * time.Second
)

// Destination is a Slack conversation: the thread the requesting turn is
// running in, or the admin's DM.
type Destination struct {
	ChannelID string
	ThreadTS  string
}

// Request is one authentication request.
type Request struct {
	// ToolName is what the agent named as needing access. It is shown to both
	// parties and is agent-supplied, so it is interpolated through the
	// templates' escaping funcs like any other untrusted value.
	ToolName string

	// Profile is the workflow to drive.
	Profile auth.Profile

	// Requester is the conversation the turn is running in. Zero on the
	// CLI/MCP path, where there is no thread to notify.
	Requester Destination

	// RequesterUserID is who triggered the turn. When it matches the admin the
	// two cards collapse into one.
	RequesterUserID string

	// Timeout bounds the whole request; zero uses DefaultTimeout.
	Timeout time.Duration
}

// Outcome is the result of a request. Exactly one of the flags is set, and only
// Authenticated permits the caller to proceed — every other terminal state
// fails the requesting tool closed.
type Outcome struct {
	Authenticated bool
	Denied        bool
	TimedOut      bool
	Cancelled     bool

	// Reason carries diagnostics for a failure, for the tool to report back to
	// the model. Empty on success.
	Reason string
}

// Flow posts authentication cards and drives one request to a terminal state.
// A single instance is shared between the tool (which calls Run and blocks) and
// the gateway (which routes clicks and submissions into it).
type Flow struct {
	client  *slacklib.LazyClient
	cards   *Renderer
	admin   string
	isAdmin func(string) bool
	now     nowFunc
	urlWait time.Duration

	mu       sync.Mutex
	sessions map[string]*session
}

// New builds a Flow. adminUser is the configured admin (the only person who may
// answer a request); isAdmin is the authorization predicate re-checked on every
// inbound click — nil falls back to comparing against adminUser.
func New(client *slacklib.LazyClient, cards *Renderer, adminUser string, isAdmin func(string) bool) *Flow {
	return &Flow{
		client:   client,
		cards:    cards,
		admin:    strings.TrimSpace(adminUser),
		isAdmin:  isAdmin,
		now:      time.Now,
		urlWait:  DefaultURLWait,
		sessions: make(map[string]*session),
	}
}

// session is the per-request rendezvous the gateway resolves clicks into.
type session struct {
	toolName  string
	needsCode bool

	primary     chan struct{}
	primaryOnce sync.Once
	denied      chan struct{}
	deniedOnce  sync.Once
	code        chan string
}

func (s *session) markPrimary() { s.primaryOnce.Do(func() { close(s.primary) }) }
func (s *session) deny()        { s.deniedOnce.Do(func() { close(s.denied) }) }

func (s *session) submitCode(code string) error {
	select {
	case s.code <- code:
		return nil
	default:
		return errors.New("authcard: a verification code is already being processed")
	}
}

// Run posts the cards, drives the authentication process, and blocks until it
// reaches a terminal state.
//
// It fails closed everywhere. No configured admin, an undeliverable card, a
// process that will not start or never prints a URL, a denial, a timeout, a
// cancelled turn — all of them return a non-authenticated Outcome (or an
// error), never a hopeful one. Success requires the process to exit cleanly.
func (f *Flow) Run(ctx context.Context, req Request) (Outcome, error) {
	if f.admin == "" {
		return Outcome{}, errors.New("authcard: no admin user is configured, so nobody can approve an authentication request")
	}
	api, err := f.client.Get()
	if err != nil {
		return Outcome{}, err
	}
	corr, err := newCorrelationID()
	if err != nil {
		return Outcome{}, err
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	attemptAt := f.now().Format(attemptFormat)

	// Collapse to a single card when the requester IS the admin, or when there
	// is no requesting thread at all (CLI/MCP). Telling someone their own
	// request has been forwarded to themselves is noise.
	collapsed := strings.TrimSpace(req.Requester.ChannelID) == "" || f.isAdminUser(req.RequesterUserID)

	var reqChannel, reqTS string
	if !collapsed {
		blocks, err := f.cards.render(RequesterTemplate, f.data(req, corr, "", attemptAt, StatePending, "", false, false))
		if err != nil {
			return Outcome{}, err
		}
		posted, err := api.PostMessage(ctx, slacklib.PostMessageParams{
			ChannelID: req.Requester.ChannelID,
			ThreadTS:  req.Requester.ThreadTS,
			Text:      fallbackText(req.ToolName),
			Blocks:    blocks,
		})
		if err != nil {
			// An undeliverable notice is not fatal on its own — the admin can
			// still authenticate — but the requester would be left staring at a
			// silent turn, so fail rather than proceed invisibly.
			return Outcome{}, fmt.Errorf("authcard: post requester notice: %w", err)
		}
		reqChannel, reqTS = posted.Channel, posted.TS
	}

	// state carries what the cards currently show, so the closer can render the
	// right terminal card wherever it is called from.
	var adminChannel, adminTS, url string
	finish := func(o Outcome, state State, reason string) (Outcome, error) {
		o.Reason = reason
		f.settle(req, corr, url, attemptAt, state, reason,
			api, reqChannel, reqTS, adminChannel, adminTS)
		return o, nil
	}

	adminChannel, err = api.OpenDM(ctx, f.admin)
	if err != nil {
		return finish(Outcome{}, StateFailed, "could not open a DM with the admin: "+err.Error())
	}

	h, err := proc.Start(ctx, req.Profile.Spec())
	if err != nil {
		return finish(Outcome{}, StateFailed, "could not start the authentication command: "+err.Error())
	}
	// Kill on every exit path — a denial, a timeout, or an interrupt must not
	// leave the auth command parked on stdin forever.
	defer h.Kill()

	url, err = f.waitForURL(ctx, h, req.Profile)
	if err != nil {
		return finish(Outcome{}, StateFailed, err.Error())
	}

	adminBlocks, err := f.cards.render(AdminTemplate, f.data(req, corr, url, attemptAt, StatePending, "", true, false))
	if err != nil {
		return finish(Outcome{}, StateFailed, err.Error())
	}
	postedAdmin, err := api.PostMessage(ctx, slacklib.PostMessageParams{
		ChannelID: adminChannel,
		Text:      fallbackText(req.ToolName),
		Blocks:    adminBlocks,
	})
	if err != nil {
		return finish(Outcome{}, StateFailed, "could not deliver the authentication card to the admin: "+err.Error())
	}
	adminChannel, adminTS = postedAdmin.Channel, postedAdmin.TS

	s := &session{
		toolName:  req.ToolName,
		needsCode: req.Profile.NeedsCode,
		primary:   make(chan struct{}),
		denied:    make(chan struct{}),
		code:      make(chan string, 1),
	}
	f.register(corr, s)
	defer f.unregister(corr)

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	// primary is nil-ed after it fires: a closed channel is always ready, so
	// leaving it in the select would spin the loop.
	primary := s.primary

	for {
		select {
		case <-primary:
			primary = nil
			// The single attempt has been spent: retire the whole actions bar
			// and reveal the footer, but keep waiting — the flow is not over
			// until the process says so.
			f.updateAdmin(ctx, api, adminChannel, adminTS,
				f.data(req, corr, url, attemptAt, StateWorking, "", false, true), req.ToolName)

		case code := <-s.code:
			if err := h.WriteLine(code); err != nil {
				return finish(Outcome{}, StateFailed, "could not hand the verification code to the authentication command: "+err.Error())
			}

		case <-s.denied:
			return finish(Outcome{Denied: true}, StateDenied, "the admin denied the authentication request")

		case <-h.Exited():
			if auth.Succeeded(h.Wait()) {
				return finish(Outcome{Authenticated: true}, StateSuccess, "")
			}
			return finish(Outcome{}, StateFailed, describeFailure(h))

		case <-timer.C:
			return finish(Outcome{TimedOut: true}, StateTimeout, "the authentication request expired before it was completed")

		case <-ctx.Done():
			return finish(Outcome{Cancelled: true}, StateFailed, "the turn was cancelled before authentication completed")
		}
	}
}

// waitForURL reads the child's output until the profile recognises a
// verification URL. Once found we stop reading; proc drops further lines rather
// than blocking the child, which is what makes abandoning the stream safe.
func (f *Flow) waitForURL(ctx context.Context, h *proc.Handle, p auth.Profile) (string, error) {
	wait := f.urlWait
	if wait <= 0 {
		wait = DefaultURLWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()

	for {
		select {
		case line, ok := <-h.Lines():
			if !ok {
				return "", fmt.Errorf("the authentication command finished without offering a sign-in link: %s", oneLine(h.Output()))
			}
			if url, found := p.ExtractURL(line.Text); found {
				return url, nil
			}
		case <-timer.C:
			return "", fmt.Errorf("the authentication command did not offer a sign-in link within %s: %s", wait, oneLine(h.Output()))
		case <-ctx.Done():
			return "", errors.New("the turn was cancelled before authentication started")
		}
	}
}

// settle writes the terminal card to both destinations. Best-effort and on a
// fresh context: the ctx that drove Run may already be cancelled on the
// interrupt path, and a card left showing live buttons would be worse than a
// missed update.
func (f *Flow) settle(req Request, corr, url, attemptAt string, state State, reason string,
	api slacklib.SlackAPI, reqChannel, reqTS, adminChannel, adminTS string) {

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if adminChannel != "" && adminTS != "" {
		f.updateAdmin(ctx, api, adminChannel, adminTS,
			f.data(req, corr, url, attemptAt, state, reason, false, true), req.ToolName)
	}
	if reqChannel == "" || reqTS == "" {
		return
	}
	// The requester never sees the failure detail: it may quote command output,
	// which is not theirs to read. Their card carries the state alone.
	blocks, err := f.cards.render(RequesterTemplate, f.data(req, corr, "", attemptAt, state, "", false, false))
	if err != nil {
		return
	}
	_, _ = api.UpdateMessage(ctx, slacklib.UpdateMessageParams{
		ChannelID: reqChannel,
		TS:        reqTS,
		Text:      fallbackText(req.ToolName),
		Blocks:    blocks,
	})
}

func (f *Flow) updateAdmin(ctx context.Context, api slacklib.SlackAPI, channel, ts string, data cardData, toolName string) {
	if channel == "" || ts == "" {
		return
	}
	blocks, err := f.cards.render(AdminTemplate, data)
	if err != nil {
		return
	}
	_, _ = api.UpdateMessage(ctx, slacklib.UpdateMessageParams{
		ChannelID: channel,
		TS:        ts,
		Text:      fallbackText(toolName),
		Blocks:    blocks,
	})
}

func (f *Flow) data(req Request, corr, url, attemptAt string, state State, reason string, showActions, showFooter bool) cardData {
	return cardData{
		ToolName:        req.ToolName,
		ProfileName:     req.Profile.Name,
		URL:             url,
		NeedsCode:       req.Profile.NeedsCode,
		RequesterUserID: req.RequesterUserID,
		AttemptAt:       attemptAt,
		State:           string(state),
		Reason:          reason,
		ShowActions:     showActions,
		ShowFooter:      showFooter,
		ShowRequester:   strings.TrimSpace(req.RequesterUserID) != "",
		ActionPrimary:   ActionID(corr, ActionPrimary),
		ActionOpen:      ActionID(corr, ActionOpen),
		ActionDeny:      ActionID(corr, ActionDeny),
	}
}

// HandleClick routes an admin click into the waiting request.
//
// Authorization is re-checked here rather than trusted from the router: the
// card lives in a DM, but an action_id is guessable and Slack will deliver a
// crafted callback from anyone the gateway admits. Only the admin can answer.
func (f *Flow) HandleClick(ctx context.Context, corr string, action Action, userID, triggerID string) error {
	if !f.isAdminUser(userID) {
		return fmt.Errorf("authcard: user %s is not the admin; auth click ignored", userID)
	}
	s, ok := f.session(corr)
	if !ok {
		return fmt.Errorf("authcard: no authentication request is waiting for %q", corr)
	}

	switch action {
	case ActionDeny:
		s.deny()
		return nil

	case ActionPrimary:
		s.markPrimary()
		if !s.needsCode {
			// Browser-only: Slack has opened the link, and the flow completes
			// when the process exits. Nothing further to do here.
			return nil
		}
		api, err := f.client.Get()
		if err != nil {
			return err
		}
		return api.OpenView(ctx, triggerID, CodeModal(corr, s.toolName))

	case ActionOpen:
		// The secondary link on a code flow. Slack already opened the URL, and
		// this is explicitly NOT the single attempt — the admin still has to
		// come back and enter the code.
		return nil
	}
	return fmt.Errorf("authcard: unknown auth action %q", action)
}

// HandleCodeSubmission routes a verification code from the modal into the
// waiting request. Admin-only, for the same reason as HandleClick.
func (f *Flow) HandleCodeSubmission(corr, code, userID string) error {
	if !f.isAdminUser(userID) {
		return fmt.Errorf("authcard: user %s is not the admin; code submission ignored", userID)
	}
	if strings.TrimSpace(code) == "" {
		return errors.New("authcard: the verification code was empty")
	}
	s, ok := f.session(corr)
	if !ok {
		return fmt.Errorf("authcard: no authentication request is waiting for %q", corr)
	}
	return s.submitCode(strings.TrimSpace(code))
}

func (f *Flow) isAdminUser(userID string) bool {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	if f.isAdmin != nil {
		return f.isAdmin(userID)
	}
	return userID == f.admin
}

func (f *Flow) register(corr string, s *session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[corr] = s
}

func (f *Flow) unregister(corr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, corr)
}

func (f *Flow) session(corr string) (*session, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[corr]
	return s, ok
}

// describeFailure turns a failed run into something worth showing the admin.
func describeFailure(h *proc.Handle) string {
	out := oneLine(h.Output())
	if out == "" {
		return "the authentication command failed"
	}
	return "the authentication command failed: " + out
}

// oneLine flattens command output to a short single line. Output goes into a
// Slack card, and an untrimmed multi-kilobyte dump would either blow the block
// limit or bury the message.
func oneLine(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return clamp(strings.Join(fields, " "), 400)
}
