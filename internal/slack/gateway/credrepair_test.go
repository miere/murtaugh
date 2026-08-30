package gateway

import (
	"errors"
	"testing"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/slack/authcard"
)

var (
	claudeAgents = map[string]config.AgentProfile{
		"claude": {ClaudeCode: &config.ClaudeCodeProfile{Command: "/usr/local/bin/claude"}},
		"native": {Native: &config.NativeProfile{Provider: "gemini", Model: "m", APIKeyEnv: "K"}},
		"acp":    {ACP: &config.ACPProfile{Command: "/opt/bridge"}},
	}
	authErr = errors.New("API Error: 401 Invalid API key · Please run /login")
)

// A non-nil Flow is needed only so the repair path is live; these tests never
// reach Run, which would require a Slack client.
func liveRepair() *credentialRepair {
	return newCredentialRepair(&authcard.Flow{}, claudeAgents)
}

func TestCredentialRepairHandlesClaudeCodeAuthFailure(t *testing.T) {
	if !liveRepair().Handles("claude", authErr) {
		t.Fatal("expected a claude_code auth failure to be handled")
	}
}

// The markers are prose. A native agent relaying its own provider's
// "invalid api key" must not trigger a Claude Code re-login.
func TestCredentialRepairIgnoresOtherBackends(t *testing.T) {
	r := liveRepair()
	for _, name := range []string{"native", "acp"} {
		if r.Handles(name, authErr) {
			t.Fatalf("agent %q is not claude_code; must not trigger a Claude Code repair", name)
		}
	}
}

func TestCredentialRepairIgnoresOrdinaryErrors(t *testing.T) {
	r := liveRepair()
	for name, err := range map[string]error{
		"nil":          nil,
		"tool ceiling": errors.New("claudecode: turn aborted mid-execution (error_max_turns)"),
		"rate limit":   errors.New("API Error: 429 rate limited"),
	} {
		t.Run(name, func(t *testing.T) {
			if r.Handles("claude", err) {
				t.Fatal("ordinary failure must be reported as-is, not as a credential problem")
			}
		})
	}
}

func TestCredentialRepairIgnoresUnknownAgent(t *testing.T) {
	if liveRepair().Handles("ghost", authErr) {
		t.Fatal("an unknown agent has no resolvable kind and must not be handled")
	}
}

// A nil repair (no auth flow wired — CLI, tests, no admin) must be inert rather
// than panic, so credential failures simply report as ordinary errors.
func TestNilCredentialRepairIsInert(t *testing.T) {
	var r *credentialRepair
	if r.Handles("claude", authErr) {
		t.Fatal("nil repair must not claim to handle anything")
	}
	if r.Request("claude") {
		t.Fatal("nil repair must not report that an admin was asked")
	}
	if got := newCredentialRepair(nil, claudeAgents); got != nil {
		t.Fatal("a nil flow must produce a nil repair, not a live one")
	}
}

// A bad credential fails EVERY concurrent turn. Without the in-flight guard the
// admin gets one card per failing conversation for one underlying problem.
func TestCredentialRepairPostsOneCardForConcurrentFailures(t *testing.T) {
	r := liveRepair()
	r.mu.Lock()
	r.inFlight = true // simulate a repair already running
	r.mu.Unlock()

	// Still true: the user is correctly told the admin has been asked...
	if !r.Request("claude") {
		t.Fatal("a second failure should still tell the user the admin was asked")
	}
	// ...and the flag is untouched, so no second flow was started.
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.inFlight {
		t.Fatal("the in-flight guard must not be cleared by a coalesced request")
	}
}

func TestFailedOnCredentialIsFalseWithoutRepair(t *testing.T) {
	h := &ChatHandler{}
	if h.failedOnCredential("claude", authErr) {
		t.Fatal("a handler with no repair wired must report failures as ordinary errors")
	}
}

func TestErrCredentialBlockedDoesNotTellUsersToRunLogin(t *testing.T) {
	// The CLI's own text says "Please run /login", which the user cannot do and
	// which is not their job — the admin owns the credential.
	msg := errCredentialBlocked.Error()
	if contains(msg, "/login") {
		t.Fatalf("user-facing message must not instruct the user to run /login: %q", msg)
	}
	if !contains(msg, "admin") {
		t.Fatalf("user-facing message should say the admin was asked: %q", msg)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
