package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/miere/murtaugh/internal/agent/claudecode"
	"github.com/miere/murtaugh/internal/auth"
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

	// mu guards inFlight. Only one repair runs at a time: a bad credential fails
	// EVERY concurrent turn, and without this the admin would get one card per
	// failing conversation for the same underlying problem.
	mu       sync.Mutex
	inFlight bool
}

// newCredentialRepair builds the repair path. A nil flow (no gateway to route
// the admin's click back from) leaves it inert.
func newCredentialRepair(flow *authcard.Flow, agents map[string]config.AgentProfile) *credentialRepair {
	if flow == nil {
		return nil
	}
	return &credentialRepair{flow: flow, agents: agents}
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
	return claudecode.IsAuthFailure(err)
}

// Request starts a repair if one is not already running, and reports whether the
// caller may tell the user that the admin has been asked.
//
// The card is driven on a background context rather than the turn's: the turn is
// already over (its error is what brought us here) and cancelling the admin's
// sign-in when the user's message finishes rendering would make the flow
// unusable.
func (c *credentialRepair) Request(agentName string) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	if c.inFlight {
		c.mu.Unlock()
		// Already asked. The caller still tells the user their turn is blocked on
		// the admin, which is true — it just does not post a second card.
		return true
	}
	c.inFlight = true
	c.mu.Unlock()

	profile, err := auth.Resolve(claudeReauthProfile, "", false)
	if err != nil {
		c.release()
		return false
	}

	go func() {
		defer c.release()
		ctx, cancel := context.WithTimeout(context.Background(), reauthTimeout)
		defer cancel()
		// The outcome is deliberately not acted on here. The flow already reports
		// success, denial and timeout on the admin's own card, and the user was
		// told when the request went out; re-announcing it into a thread whose turn
		// has long since ended would be noise.
		_, _ = c.flow.Run(ctx, authcard.Request{
			ToolName: "Claude Code (agent " + agentName + ")",
			Profile:  profile,
			Timeout:  reauthTimeout,
		})
	}()
	return true
}

func (c *credentialRepair) release() {
	c.mu.Lock()
	c.inFlight = false
	c.mu.Unlock()
}
