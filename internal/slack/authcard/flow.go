package authcard

import (
	"context"
	"errors"
	"fmt"
	"os"
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

	// Env is the requesting agent's own environment, as KEY=VALUE pairs, layered
	// over the daemon's when the authentication command is spawned.
	//
	// Without it the sign-in runs in the DAEMON's environment while the agent
	// that asked for it runs in its own: `gcloud auth login` writes to
	// ~/.config/gcloud, and the agent — whose CLOUDSDK_CONFIG points into its
	// workspace — still finds nothing. The flow reports success and the agent
	// cannot proceed, which is the worst shape a failure can take.
	//
	// Empty on the CLI/MCP path and for native agents, which have no process
	// environment of their own.
	Env []string

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
	allowed func(string) bool
	now     nowFunc
	urlWait time.Duration

	mu       sync.Mutex
	sessions map[string]*Card
}

// New takes adminUser as the fallback recipient, because a sign-in the gateway
// runs itself has no node owner to send it to.
func New(client *slacklib.LazyClient, cards *Renderer, adminUser string, isAdmin func(string) bool) *Flow {
	return &Flow{
		client:   client,
		cards:    cards,
		admin:    strings.TrimSpace(adminUser),
		isAdmin:  isAdmin,
		now:      time.Now,
		urlWait:  DefaultURLWait,
		sessions: make(map[string]*Card),
	}
}

// commandSpec renders the process to spawn for a request, along with the
// cleanup its environment requires.
//
// The environment is layered rather than replaced: the authentication CLI needs
// everything the daemon has (PATH, HOME, the login keychain's reach) plus the
// requesting agent's redirections on top. A nil Spec.Env inherits the daemon's
// outright, which is the right answer for a request carrying none and avoids
// materialising a copy of os.Environ for every caller.
//
// A profile that suppresses the browser adds one more layer, and it goes on TOP
// of the agent's: the guard is a safety control, and an agent that could restore
// PATH could put a consent window back on the host desktop. It is also computed
// against the already-merged environment, so it prepends to the PATH the child
// will really see rather than to the daemon's copy.
//
// The returned cleanup is always non-nil and safe to defer, including on error.
func commandSpec(req Request) (proc.Spec, func(), error) {
	spec := req.Profile.Spec()
	cleanup := func() {}

	overrides := req.Env
	if req.Profile.SuppressBrowser {
		guard, err := auth.NewBrowserGuard()
		if err != nil {
			return proc.Spec{}, cleanup, err
		}
		cleanup = func() { _ = guard.Close() }
		effective := proc.MergeEnv(os.Environ(), overrides)
		overrides = append(append([]string(nil), overrides...), guard.Overrides(effective)...)
	}

	if len(overrides) > 0 {
		spec.Env = proc.MergeEnv(os.Environ(), overrides)
	}
	return spec, cleanup, nil
}

// Run posts the cards, drives the authentication process, and blocks until it
// reaches a terminal state.
//
// It fails closed everywhere. No configured admin, an undeliverable card, a
// process that will not start or never prints a URL, a denial, a timeout, a
// cancelled turn — all of them return a non-authenticated Outcome (or an
// error), never a hopeful one. Success requires the process to exit cleanly.
func (f *Flow) Run(ctx context.Context, req Request) (Outcome, error) {
	if f.adminUser() == "" {
		return Outcome{}, errors.New("authcard: no admin user is configured, so nobody can approve an authentication request")
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	card, err := f.open(ctx, Showing{
		ToolName:        req.ToolName,
		ProfileName:     req.Profile.Name,
		NeedsCode:       req.Profile.NeedsCode,
		Requester:       req.Requester,
		RequesterUserID: req.RequesterUserID,
	})
	if card == nil {
		return Outcome{}, err
	}
	finish := func(o Outcome, state State, reason string) (Outcome, error) {
		o.Reason = reason
		card.Settle(state, reason)
		return o, nil
	}
	if err != nil {
		return finish(Outcome{}, StateFailed, err.Error())
	}

	spec, cleanupSpec, err := commandSpec(req)
	defer cleanupSpec()
	if err != nil {
		return finish(Outcome{}, StateFailed, "could not prepare the authentication command: "+err.Error())
	}
	h, err := proc.Start(ctx, spec)
	if err != nil {
		return finish(Outcome{}, StateFailed, "could not start the authentication command: "+err.Error())
	}
	// Kill on every exit path — a denial, a timeout, or an interrupt must not
	// leave the auth command parked on stdin forever.
	defer h.Kill()

	url, err := f.waitForURL(ctx, h, req.Profile)
	if err != nil {
		return finish(Outcome{}, StateFailed, err.Error())
	}
	if err := card.post(ctx, url); err != nil {
		return finish(Outcome{}, StateFailed, err.Error())
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case r := <-card.Replies():
			switch r.Kind {
			case ReplyCode:
				if err := h.WriteLine(r.Code); err != nil {
					return finish(Outcome{}, StateFailed, "could not hand the verification code to the authentication command: "+err.Error())
				}
			case ReplyDenied:
				return finish(Outcome{Denied: true}, StateDenied, "the admin denied the authentication request")
			default:
				return finish(Outcome{Cancelled: true}, StateFailed, r.Reason)
			}

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
				return "", fmt.Errorf("the authentication command finished without offering a sign-in link: %s%s",
					oneLine(h.Output()), driftSuffix(ctx, p))
			}
			if url, found := p.ExtractURL(line.Text); found {
				return url, nil
			}
		case <-timer.C:
			return "", fmt.Errorf("the authentication command did not offer a sign-in link within %s: %s%s",
				wait, oneLine(h.Output()), driftSuffix(ctx, p))
		case <-ctx.Done():
			return "", errors.New("the turn was cancelled before authentication started")
		}
	}
}

// driftSuffix renders the profile's CLI version drift as a trailing clause, or
// "" when there is nothing to say. Only ever called on a failure path — the
// probe spawns a process, and the happy path has no use for it.
func driftSuffix(ctx context.Context, p auth.Profile) string {
	if note := p.VersionDrift(ctx); note != "" {
		return " (" + note + ")"
	}
	return ""
}

// SetAdmin installs the resolved admin identity.
//
// The gateway resolves configuration.admin_user from a handle ("@miere") to a
// Slack user ID at startup and rewrites its own copy of the access config; the
// value this Flow was constructed with is the unresolved one. Without this the
// DM would be opened against a handle and fail. The gateway calls it once
// resolution has happened; empty/nil arguments are ignored so a lockdown
// configuration cannot accidentally clear a good value.
func (f *Flow) SetAdmin(userID string, isAdmin func(string) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u := strings.TrimSpace(userID); u != "" {
		f.admin = u
	}
	if isAdmin != nil {
		f.isAdmin = isAdmin
	}
}

// adminUser returns the resolved admin, or "" when none is configured.
func (f *Flow) adminUser() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.admin
}

func (f *Flow) isAdminUser(userID string) bool {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	f.mu.Lock()
	predicate, admin := f.isAdmin, f.admin
	f.mu.Unlock()

	if predicate != nil {
		return predicate(userID)
	}
	return userID == admin
}

// SetAuthorised installs who may use the gateway at all. It is asked on every
// click, so access withdrawn while a card is open stops the sign-in.
func (f *Flow) SetAuthorised(allowed func(string) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allowed = allowed
}

func (f *Flow) authorised(userID string) bool {
	f.mu.Lock()
	allowed := f.allowed
	f.mu.Unlock()
	if allowed != nil {
		return allowed(strings.TrimSpace(userID))
	}
	return f.isAdminUser(userID)
}

func (f *Flow) register(corr string, s *Card) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[corr] = s
}

func (f *Flow) unregister(corr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, corr)
}

func (f *Flow) card(corr string) (*Card, bool) {
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
