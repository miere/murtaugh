package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agent/remote"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/nodelink"
	"github.com/miere/murtaugh/internal/nodeserve"
)

type failingAgent struct {
	onStart bool
}

var errLogin = errors.New("claudecode: session failed: API Error: 401 Invalid API key · Please run /login")

func (failingAgent) Initialize(context.Context) error { return nil }
func (failingAgent) NewSession(context.Context, agent.SessionMetadata) (agent.Session, error) {
	return agent.Session{ID: "s1"}, nil
}
func (a failingAgent) Prompt(context.Context, string, agent.PromptRequest) (<-chan agent.Event, error) {
	if a.onStart {
		return nil, errLogin
	}
	events := make(chan agent.Event, 1)
	events <- agent.Event{Type: agent.EventError, Error: errLogin}
	close(events)
	return events, nil
}
func (failingAgent) Cancel(context.Context, string) error { return nil }
func (failingAgent) Close() error                         { return nil }

func fakeClaude(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\necho \"Visit https://claude.com/cai/oauth/authorize?code=true&x=1\"\nread code\n[ \"$code\" = GOOD ]\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return path
}

type drawFunc = func(ctx context.Context, prompt *agent.SignInPrompt, updates <-chan agent.SignInSettled, shown func(error))

func repairRig(t *testing.T, onStart bool, draw drawFunc) (*repairer, func() error) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{Agents: map[string]config.AgentProfile{"coder": {ClaudeCode: &config.ClaudeCodeProfile{Command: fakeClaude(t)}}}}
	signIns := nodeserve.NewSignIns(logger)
	repair := newRepairer(cfg, "coder", signIns, logger)
	gatewaySide, nodeSide := nodelink.Pipe(32)
	gateway := remote.New(gatewaySide, remote.Options{Logger: logger, Owner: "UOWNER", SignIns: draw})
	t.Cleanup(func() { _ = gateway.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = nodeserve.Serve(ctx, nodeSide, failingAgent{onStart: onStart}, nodeserve.Options{Logger: logger, SignIns: signIns, Failed: repair.failed})
	}()
	return repair, func() error {
		session, err := gateway.NewSession(ctx, agent.SessionMetadata{})
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		events, err := gateway.Prompt(ctx, session.ID, agent.PromptRequest{Text: "hi"})
		if err != nil {
			return err
		}
		var failure error
		for ev := range events {
			if ev.Type == agent.EventError {
				failure = ev.Error
			}
		}
		return failure
	}
}

func TestANodeAsksItsOwnerToSignInWhenItsCredentialIsRejected(t *testing.T) {
	for name, onStart := range map[string]bool{"mid-turn": false, "on session start": true} {
		t.Run(name, func(t *testing.T) {
			drawn := make(chan *agent.SignInPrompt, 4)
			settled := make(chan agent.SignInState, 8)
			_, turn := repairRig(t, onStart, func(ctx context.Context, prompt *agent.SignInPrompt, updates <-chan agent.SignInSettled, shown func(error)) {
				drawn <- prompt
				shown(nil)
				for {
					select {
					case u := <-updates:
						settled <- u.State
						if u.State == agent.SignInConfirming {
							prompt.Answer <- agent.DisplayAnswer{Outcome: agent.DisplayApproved}
						}
						if u.State.Terminal() {
							return
						}
					case <-ctx.Done():
						return
					}
				}
			})

			if err := turn(); !errors.Is(err, agent.ErrCredentialRejected) {
				t.Fatalf("the gateway saw %v; it cannot tell the owner has a sign-in open", err)
			}
			var prompt *agent.SignInPrompt
			select {
			case prompt = <-drawn:
			case <-time.After(10 * time.Second):
				t.Fatal("the node never asked its owner to sign in")
			}
			if prompt.Owner != "UOWNER" || prompt.Request.Profile != "claude-code" || prompt.Request.URL != "https://claude.com/cai/oauth/authorize?code=true&x=1" {
				t.Fatalf("the sign-in went out as %+v for %q", prompt.Request, prompt.Owner)
			}

			if err := turn(); !errors.Is(err, agent.ErrCredentialRejected) {
				t.Fatalf("a second failure reached the gateway as %v", err)
			}
			select {
			case again := <-drawn:
				t.Fatalf("a second failure started a second sign-in: %+v", again.Request)
			case <-time.After(300 * time.Millisecond):
			}

			prompt.Answer <- agent.DisplayAnswer{Outcome: agent.DisplayAnswered, Code: "GOOD", UserID: "UOWNER"}
			deadline := time.After(10 * time.Second)
			for state := agent.SignInWorking; state != agent.SignInSuccess; {
				select {
				case state = <-settled:
					if state.Terminal() && state != agent.SignInSuccess {
						t.Fatalf("the sign-in ended as %q", state)
					}
				case <-deadline:
					t.Fatal("the sign-in never finished")
				}
			}
		})
	}
}

func TestANodeWhoseOwnerCannotBeAskedNeitherSaysSoNorAsksAgainEveryTurn(t *testing.T) {
	var asked atomic.Int32
	_, turn := repairRig(t, false, func(_ context.Context, _ *agent.SignInPrompt, _ <-chan agent.SignInSettled, shown func(error)) {
		asked.Add(1)
		shown(errors.New("the owner of this machine may not use this gateway"))
	})
	for i := range 3 {
		err := turn()
		if errors.Is(err, agent.ErrCredentialRejected) {
			t.Fatalf("turn %d: the gateway was told the owner has a sign-in open when the gateway refused it", i)
		}
		if err == nil || !strings.Contains(err.Error(), "no sign-in could be put in front of its owner") {
			t.Fatalf("turn %d: the user was told %v", i, err)
		}
	}
	if got := asked.Load(); got != 1 {
		t.Fatalf("the gateway was asked %d times after refusing the first; the node should wait before asking again", got)
	}
}

func TestANodeLeavesOtherFailuresAlone(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	signIns := nodeserve.NewSignIns(logger)
	native := newRepairer(config.Config{Agents: map[string]config.AgentProfile{"coder": {}}}, "coder", signIns, logger)
	if err := native.failed(errLogin); errors.Is(err, agent.ErrCredentialRejected) {
		t.Fatal("a native agent's failure was taken for a Claude Code credential")
	}
	claude := newRepairer(config.Config{Agents: map[string]config.AgentProfile{"coder": {ClaudeCode: &config.ClaudeCodeProfile{Command: fakeClaude(t)}}}}, "coder", signIns, logger)
	ordinary := errors.New("API Error: 429 rate limited")
	if err := claude.failed(ordinary); err != ordinary {
		t.Fatalf("an ordinary failure came back as %v", err)
	}
}

func TestAnAskToSignInAgainReplacesTheSignInAlreadyOpen(t *testing.T) {
	var none *repairer
	if got, _ := none.renew(context.Background()); got.Status != string(agentruntime.RenewalNothingToRenew) {
		t.Fatalf("a node with no claude_code agent answered %q", got.Status)
	}

	type drawing struct {
		prompt *agent.SignInPrompt
		ended  chan agent.SignInState
	}
	drawn := make(chan drawing, 4)
	repair, _ := repairRig(t, false, func(ctx context.Context, prompt *agent.SignInPrompt, updates <-chan agent.SignInSettled, shown func(error)) {
		d := drawing{prompt: prompt, ended: make(chan agent.SignInState, 1)}
		drawn <- d
		shown(nil)
		for {
			select {
			case u := <-updates:
				if u.State.Terminal() {
					d.ended <- u.State
					return
				}
			case <-ctx.Done():
				return
			}
		}
	})
	await := func() drawing {
		select {
		case d := <-drawn:
			return d
		case <-time.After(10 * time.Second):
			t.Fatal("no sign-in was drawn")
			return drawing{}
		}
	}

	if got, _ := repair.renew(context.Background()); got.Status != string(agentruntime.RenewalStarted) {
		t.Fatalf("an idle node answered %q", got.Status)
	}
	first := await()
	if got, _ := repair.renew(context.Background()); got.Status != string(agentruntime.RenewalStarted) {
		t.Fatalf("a node with a sign-in open answered %q; asking again must replace it", got.Status)
	}
	select {
	case state := <-first.ended:
		if state != agent.SignInCancelled {
			t.Fatalf("the replaced sign-in ended as %q", state)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the replaced sign-in was never withdrawn")
	}
	if second := await(); second.prompt == first.prompt {
		t.Fatal("no fresh sign-in replaced the old one")
	}

	stuck := &repairRun{cancel: func() {}, shown: make(chan struct{}), done: make(chan struct{})}
	repair.mu.Lock()
	previous := repair.running
	repair.running = stuck
	repair.mu.Unlock()
	if got, _ := repair.renew(context.Background()); got.Status != string(agentruntime.RenewalAlreadyRunning) {
		t.Fatalf("a node whose sign-in would not stop answered %q", got.Status)
	}
	repair.mu.Lock()
	repair.running = previous
	repair.mu.Unlock()
}
