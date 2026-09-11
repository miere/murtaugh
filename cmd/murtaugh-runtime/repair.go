package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/auth"
	"github.com/miere/murtaugh/internal/claudeauth"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/credwarden"
	"github.com/miere/murtaugh/internal/nodeserve"
	authrequest "github.com/miere/murtaugh/internal/tools/auth/request"
)

const (
	repairCooldown = 10 * time.Minute
	repairShowWait = 10 * time.Second
	repairDrain    = time.Second
)

type repairer struct {
	signIns  *nodeserve.SignIns
	agent    string
	identity credwarden.Identity
	log      *slog.Logger

	mu            sync.Mutex
	running       *repairRun
	cooldownSince time.Time
}

type repairRun struct {
	cancel context.CancelFunc
	shown  chan struct{}
	done   chan struct{}
}

type shownDisplay struct {
	authrequest.Display
	once  sync.Once
	shown chan struct{}
}

func (d *shownDisplay) SignIn(ctx context.Context, req agent.SignInRequest) (*agent.SignInPrompt, bool) {
	prompt, ok := d.Display.SignIn(ctx, req)
	if ok {
		d.once.Do(func() { close(d.shown) })
	}
	return prompt, ok
}

func newRepairer(cfg config.Config, agentName string, signIns *nodeserve.SignIns, logger *slog.Logger) *repairer {
	ids := servedIdentities(cfg, agentName)
	if len(ids) != 1 || signIns == nil {
		return nil
	}
	return &repairer{signIns: signIns, agent: agentName, identity: ids[0], log: logger}
}

func (r *repairer) failed(err error) error {
	if r == nil || errors.Is(err, agent.ErrCredentialRejected) || !claudeauth.IsAuthFailure(err) {
		return err
	}
	if run := r.current(); run != nil && run.awaitShown(repairShowWait) {
		return fmt.Errorf("%w: %w", agent.ErrCredentialRejected, err)
	}
	return fmt.Errorf("the Claude Code credential on this machine was rejected, and no sign-in could be put in front of its owner: %w", err)
}

func (r *repairer) renew(context.Context) (agentwire.CredentialRenewal, error) {
	if r == nil {
		return agentwire.CredentialRenewal{Status: string(agentruntime.RenewalNothingToRenew)}, nil
	}
	r.mu.Lock()
	previous := r.running
	r.mu.Unlock()
	if previous != nil {
		previous.cancel()
		select {
		case <-previous.done:
		case <-time.After(repairDrain):
			r.log.Warn("an earlier Claude Code sign-in would not stop; refusing to start a second", "agent", r.agent)
			return agentwire.CredentialRenewal{Status: string(agentruntime.RenewalAlreadyRunning)}, nil
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running != nil {
		return agentwire.CredentialRenewal{Status: string(agentruntime.RenewalAlreadyRunning)}, nil
	}
	r.startLocked()
	return agentwire.CredentialRenewal{Status: string(agentruntime.RenewalStarted)}, nil
}

func (r *repairer) current() *repairRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running != nil {
		return r.running
	}
	if !r.cooldownSince.IsZero() && time.Since(r.cooldownSince) < repairCooldown {
		return nil
	}
	return r.startLocked()
}

func (r *repairer) startLocked() *repairRun {
	ctx, cancel := context.WithCancel(context.Background())
	if r.identity.Home != "" {
		ctx = agent.WithTurnEnv(ctx, []string{"HOME=" + r.identity.Home})
	}
	run := &repairRun{cancel: cancel, shown: make(chan struct{}), done: make(chan struct{})}
	r.running = run
	r.cooldownSince = time.Time{}

	profile, _ := auth.Lookup("claude-code")
	profile.Command = r.identity.Command
	display := &shownDisplay{Display: r.signIns, shown: run.shown}
	r.log.Warn("asking this node's owner to sign Claude Code in again", "agent", r.agent, "credential", r.identity.String())
	go func() {
		defer close(run.done)
		defer cancel()
		err := authrequest.Repair(ctx, display, "Claude Code (agent "+r.agent+")", profile)
		r.mu.Lock()
		if r.running == run {
			r.running = nil
			if err != nil && ctx.Err() == nil {
				r.cooldownSince = time.Now()
			}
		}
		r.mu.Unlock()
		if err != nil {
			r.log.Warn("the Claude Code sign-in did not complete", "agent", r.agent, "error", err)
			return
		}
		r.log.Info("the Claude Code sign-in succeeded", "agent", r.agent)
	}()
	return run
}

func (run *repairRun) awaitShown(wait time.Duration) bool {
	select {
	case <-run.done:
		return false
	default:
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-run.shown:
		return true
	case <-run.done:
		return false
	case <-timer.C:
		return false
	}
}
