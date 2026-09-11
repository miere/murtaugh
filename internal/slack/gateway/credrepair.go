package gateway

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/auth"
	"github.com/miere/murtaugh/internal/claudeauth"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/slack/authcard"
)

// claudeReauthProfile is the built-in auth profile that repairs the Claude Code
// credential (see internal/auth).
const claudeReauthProfile = "claude-code"

// reauthTimeout bounds one unprompted repair. It is longer than the auth card's
// own default because nobody asked for this one: the admin has to notice a DM
// they were not expecting, open a browser and sign in.
const reauthTimeout = 20 * time.Minute

// restartDrain bounds how long Restart waits for a cancelled repair to let go.
//
// A repair that will not stop is the one case where refusing to start another is
// right: two `claude auth login` processes against one credential store is the
// rotation race this whole area exists to avoid, and it is worse than making the
// admin ask again.
const restartDrain = 5 * time.Second

// repairStatus is what a repair request actually did.
//
// It replaces a bool that conflated "started" with "one was already running",
// which is how, on 2026-09-07, three consecutive `/murtaugh auth login` commands
// were each answered with "the card is on its way to your DMs" while nothing
// ran. The admin spent 38 minutes locked out re-issuing a command that had been
// a no-op since the first one.
type repairStatus int

const (
	// repairFailed means nothing is running and nothing was started.
	repairFailed repairStatus = iota
	// repairStarted means a fresh sign-in is now under way.
	repairStarted
	// repairAlreadyRunning means an earlier request still holds the flow. The
	// caller has not been given a new card, and must not claim otherwise.
	repairAlreadyRunning
)

// started reports whether a repair is under way as a result of this call or an
// earlier one — the question the chat path asks, since either way the user's
// turn is genuinely blocked on the admin.
func (s repairStatus) started() bool {
	return s == repairStarted || s == repairAlreadyRunning
}

// String keeps the status legible in a log line, where the whole point is that
// somebody reading it can tell "started" from "already running".
func (s repairStatus) String() string {
	switch s {
	case repairStarted:
		return "started"
	case repairAlreadyRunning:
		return "already-running"
	default:
		return "failed"
	}
}

// credentialRepair asks the admin to re-authenticate a Claude Code credential
// that has been rejected mid-turn.
//
// It exists because auth.request cannot cover this case. That tool is called BY
// an agent, from inside a turn — but when the credential itself is bad the agent
// cannot run at all, so there is no turn to call it from. The gateway has to
// notice and ask on the agent's behalf.
type credentialRepair struct {
	flow   *authcard.Flow
	agents map[string]config.AgentProfile
	logger *slog.Logger

	// mu guards the in-flight run. Only one repair runs at a time: a bad
	// credential fails EVERY concurrent turn, and without this the admin would
	// get one card per failing conversation for the same underlying problem.
	mu        sync.Mutex
	inFlight  bool
	cancel    context.CancelFunc
	done      chan struct{}
	startedAt time.Time

	// drain is how long Restart waits for a cancelled run to let go. A field
	// only so tests need not spend restartDrain proving the refusal.
	drain time.Duration
}

// newCredentialRepair builds the repair path. A nil flow (no gateway to route
// the admin's click back from) leaves it inert.
func newCredentialRepair(flow *authcard.Flow, agents map[string]config.AgentProfile, logger *slog.Logger) *credentialRepair {
	if flow == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &credentialRepair{flow: flow, agents: agents, logger: logger, drain: restartDrain}
}

func localCredentialRepair(flow *authcard.Flow, agents map[string]config.AgentProfile, runtime agentruntime.Runtime, logger *slog.Logger) *credentialRepair {
	if !runtime.InProcess {
		return nil
	}
	return newCredentialRepair(flow, agents, logger)
}

// Handles reports whether this error is a Claude Code credential failure on a
// claude_code agent — the only case this path acts on.
//
// The agent kind is checked as well as the error text because the markers are
// prose: a native agent relaying an upstream provider's "invalid api key" must
// not trigger a Claude Code re-login.
func (c *credentialRepair) Handles(agentName string, err error) bool {
	if c == nil || err == nil {
		return false
	}
	profile, ok := c.agents[agentName]
	if !ok || profile.ResolvedKind() != config.AgentKindClaudeCode {
		return false
	}
	return claudeauth.IsAuthFailure(err)
}

// Request starts a repair unless one is already running.
//
// This is the automatic path, driven by a failing turn. Deferring to a run
// already in progress is the right answer here: a bad credential fails every
// concurrent conversation, and one card per failure would bury the admin under
// cards for a single underlying problem.
func (c *credentialRepair) Request(agentName string) repairStatus {
	if c == nil {
		return repairFailed
	}
	c.mu.Lock()
	running := c.inFlight
	c.mu.Unlock()
	if running {
		return repairAlreadyRunning
	}
	return c.start(agentName)
}

// Restart cancels any repair in progress and starts a fresh one, reporting how
// old the cancelled attempt was.
//
// This is the ADMIN path, and it deliberately behaves differently from Request.
// Somebody typing the auth verb has usually done so because the last attempt
// visibly went nowhere; deferring to that attempt leaves them re-issuing a
// command that cannot do anything until a 20-minute lease expires. Preempting
// costs an in-progress sign-in the admin could still have finished — which is
// why the returned age is reported back to them rather than swallowed.
func (c *credentialRepair) Restart(agentName string) (repairStatus, time.Duration) {
	if c == nil {
		return repairFailed, 0
	}
	c.mu.Lock()
	cancel, done, since := c.cancel, c.done, c.startedAt
	c.mu.Unlock()

	var replaced time.Duration
	if cancel != nil && done != nil {
		replaced = time.Since(since)
		cancel()
		select {
		case <-done:
		case <-time.After(c.drain):
			c.logger.Warn("previous claude_code re-authentication would not stop; refusing to start a second",
				"agent", agentName, "running_for", replaced)
			return repairFailed, replaced
		}
	}
	return c.start(agentName), replaced
}

// InFlightFor reports how long the current repair has been running, and whether
// there is one at all.
func (c *credentialRepair) InFlightFor() (time.Duration, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.inFlight {
		return 0, false
	}
	return time.Since(c.startedAt), true
}

// start launches the flow. The card is driven on a background context rather
// than a turn's: the turn is already over (its error is what brought us here)
// and cancelling the admin's sign-in when the user's message finishes rendering
// would make the flow unusable.
func (c *credentialRepair) start(agentName string) repairStatus {
	profile, err := auth.Resolve(claudeReauthProfile, "", false)
	if err != nil {
		c.logger.Error("could not resolve the claude_code re-authentication profile", "error", err)
		return repairFailed
	}

	c.mu.Lock()
	if c.inFlight {
		// Lost the race to a concurrent caller. Reporting it as started would
		// put us back to promising a card nobody is going to post.
		c.mu.Unlock()
		return repairAlreadyRunning
	}
	ctx, cancel := context.WithTimeout(context.Background(), reauthTimeout)
	done := make(chan struct{})
	c.inFlight, c.cancel, c.done, c.startedAt = true, cancel, done, time.Now()
	c.mu.Unlock()

	go func() {
		// LIFO: release first, so anything waiting on done already sees the slot
		// free.
		defer close(done)
		defer cancel()
		defer c.release()

		outcome, err := c.flow.Run(ctx, authcard.Request{
			ToolName: "Claude Code (agent " + agentName + ")",
			Profile:  profile,
			Timeout:  reauthTimeout,
		})
		c.logOutcome(agentName, outcome, err)
	}()
	return repairStarted
}

// logOutcome records how the repair ended.
//
// The outcome used to be discarded outright, on the reasoning that the flow
// already reports success, denial and timeout on the admin's own card. That
// holds only while there IS a card: a request that dies before one is posted
// reported to nobody, and the daemon log — the first place anyone looks — said
// nothing at all.
func (c *credentialRepair) logOutcome(agentName string, o authcard.Outcome, err error) {
	switch {
	case err != nil:
		c.logger.Warn("claude_code re-authentication could not run", "agent", agentName, "error", err)
	case o.Authenticated:
		c.logger.Info("claude_code re-authentication succeeded", "agent", agentName)
	default:
		c.logger.Warn("claude_code re-authentication did not complete",
			"agent", agentName,
			"denied", o.Denied,
			"timed_out", o.TimedOut,
			"cancelled", o.Cancelled,
			"reason", o.Reason)
	}
}

func (c *credentialRepair) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight = false
	c.cancel = nil
	c.done = nil
}
