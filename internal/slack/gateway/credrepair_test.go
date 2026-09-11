package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentruntime"
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
	return newCredentialRepair(&authcard.Flow{}, claudeAgents,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	if r.Request("claude").started() {
		t.Fatal("nil repair must not report that an admin was asked")
	}
	if status, _ := r.Restart("claude"); status != repairFailed {
		t.Fatalf("nil repair Restart = %v, want failed", status)
	}
	if _, running := r.InFlightFor(); running {
		t.Fatal("nil repair must not report a run in flight")
	}
	if got := newCredentialRepair(nil, claudeAgents, nil); got != nil {
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
	status := r.Request("claude")
	if !status.started() {
		t.Fatal("a second failure should still tell the user the admin was asked")
	}
	// ...but it must say WHICH truth it is telling. Reporting this as "started"
	// is the bug that let three slash commands each claim a card was on its way.
	if status != repairAlreadyRunning {
		t.Fatalf("Request = %v, want already-running so the caller cannot claim a fresh card", status)
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

// settle waits for any in-flight repair goroutine to finish, so a test does not
// leave one running past its own end.
func settle(t *testing.T, r *credentialRepair) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, running := r.InFlightFor(); !running {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("a repair was still in flight after 2s")
}

// TestRestartPreemptsAnAttemptTheAdminGaveUpOn is the 2026-09-07 fix. The admin
// typed the auth verb three times in four minutes; each was swallowed by a lease
// held for twenty, and each was answered "the card is on its way to your DMs".
func TestRestartPreemptsAnAttemptTheAdminGaveUpOn(t *testing.T) {
	r := liveRepair()

	// Stand in for a repair that is under way and going nowhere.
	cancelled := make(chan struct{})
	done := make(chan struct{})
	r.mu.Lock()
	r.inFlight = true
	r.done = done
	r.startedAt = time.Now().Add(-3 * time.Minute)
	r.cancel = func() {
		close(cancelled)
		r.release()
		close(done)
	}
	r.mu.Unlock()

	status, replaced := r.Restart("manual")
	defer settle(t, r)

	select {
	case <-cancelled:
	default:
		t.Fatal("Restart did not cancel the attempt already in progress")
	}
	if status != repairStarted {
		t.Fatalf("Restart = %v, want a fresh sign-in", status)
	}
	// The admin is told what it cost them, rather than having an in-progress
	// sign-in vanish silently.
	if replaced < 2*time.Minute {
		t.Fatalf("replaced = %v, want the age of the cancelled attempt", replaced)
	}
}

// A previous run that will not let go is the one case where refusing is right:
// two `claude auth login` processes against one credential store is the
// rotation race this whole area exists to avoid.
func TestRestartRefusesWhenThePreviousRunWillNotStop(t *testing.T) {
	r := liveRepair()
	r.drain = 20 * time.Millisecond

	r.mu.Lock()
	r.inFlight = true
	r.done = make(chan struct{}) // never closed
	r.cancel = func() {}
	r.startedAt = time.Now()
	r.mu.Unlock()

	if status, _ := r.Restart("manual"); status != repairFailed {
		t.Fatalf("Restart = %v, want failed rather than a second concurrent sign-in", status)
	}
}

// Restart on an idle repair is just a start, with nothing to report as replaced.
func TestRestartFromIdleStartsCleanly(t *testing.T) {
	r := liveRepair()
	status, replaced := r.Restart("manual")
	defer settle(t, r)

	if status != repairStarted {
		t.Fatalf("Restart = %v, want started", status)
	}
	if replaced != 0 {
		t.Fatalf("replaced = %v, want zero when nothing was cancelled", replaced)
	}
}

// The automatic path still starts one when idle — the coalescing is only for
// the second and later failures.
func TestRequestFromIdleStarts(t *testing.T) {
	r := liveRepair()
	status := r.Request("claude")
	defer settle(t, r)

	if status != repairStarted {
		t.Fatalf("Request = %v, want started", status)
	}
}

// The statuses have to read as themselves in a log line; that is most of why
// they replaced a bool.
func TestRepairStatusIsLegible(t *testing.T) {
	for status, want := range map[repairStatus]string{
		repairStarted:        "started",
		repairAlreadyRunning: "already-running",
		repairFailed:         "failed",
	} {
		if got := status.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}

type rejectedOnNode struct{ onStart bool }

func (s rejectedOnNode) Prompt(context.Context, agent.ConversationKey, agent.SessionMetadata, agent.PromptRequest) (<-chan agent.Event, error) {
	rejected := fmt.Errorf("%w: %w", agent.ErrCredentialRejected, authErr)
	if s.onStart {
		return nil, rejected
	}
	ch := make(chan agent.Event, 1)
	ch <- agent.Event{Type: agent.EventError, Error: rejected}
	close(ch)
	return ch, nil
}
func (rejectedOnNode) Lookup(agent.ConversationKey) (string, bool) { return "", false }
func (rejectedOnNode) Cancel(context.Context, string) error        { return nil }

func TestACredentialFailureANodeIsRepairingStartsNothingOnTheGateway(t *testing.T) {
	for name, onStart := range map[string]bool{"mid-turn": false, "on session start": true} {
		t.Run(name, func(t *testing.T) {
			api := &fakeStreamAPI{}
			repair := liveRepair()
			handler := NewChatHandler(api, map[string]ChatSessionManager{"claude": rejectedOnNode{onStart: onStart}},
				func(ChatRequest) ChatRoute { return ChatRoute{Agent: "claude", ReplyOnThread: true} }, time.Hour, 1, nil).
				WithCredentialRepair(repair)
			_ = handler.handleResolving(context.Background(), ChatRequest{TeamID: "T1", ChannelID: "C1", UserID: "U1", MessageTS: "1.1", Text: "hi", Source: "test"})

			if _, running := repair.InFlightFor(); running {
				t.Fatal("the gateway started a sign-in of its own for a credential the node is already repairing")
			}
			said := strings.Join(api.messageTexts(), " ")
			if !strings.Contains(said, "owner has been sent a sign-in") || strings.Contains(said, "/login") {
				t.Fatalf("the user was told %q", said)
			}
		})
	}
}

func TestOnlyAGatewayRunningItsOwnAgentsRepairsCredentials(t *testing.T) {
	build := func(runtime agentruntime.Runtime) *Gateway {
		return New(config.Config{
			OAuth:  config.OAuthConfig{AppToken: "xapp-test", BotToken: "xoxb-test"},
			Agents: claudeAgents,
		}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, &authcard.Flow{}, nil,
			func(agentruntime.Hooks) agentruntime.Runtime { return runtime })
	}
	if g := build(agentruntime.Runtime{}); g.credRepair != nil {
		t.Fatal("a gateway whose agents run on nodes would start a sign-in on its own machine")
	}
	if g := build(agentruntime.Runtime{InProcess: true}); g.credRepair == nil {
		t.Fatal("a gateway running its own claude_code agents cannot repair their credential")
	}
}
