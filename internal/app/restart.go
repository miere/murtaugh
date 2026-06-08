package app

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"
)

// RestartSource identifies the origin of a restart request and is recorded
// in the audit log so the operator can tell self-service restarts apart
// from administrative ones.
type RestartSource string

const (
	// RestartSourceSlash is a `/murtaugh restart` slash command.
	RestartSourceSlash RestartSource = "slash"
	// RestartSourceInteractive is a Block Kit button or view submission.
	RestartSourceInteractive RestartSource = "interactive"
)

// RestartRequest carries the audit metadata for a single restart attempt.
// Source is required; the remaining fields are best-effort and may be
// empty for triggers that lack a Slack context (e.g. future CLI hooks).
type RestartRequest struct {
	Source  RestartSource
	UserID  string
	Channel string
	Reason  string
}

const (
	defaultRestartCooldown = 10 * time.Second
	defaultRestartGrace    = 5 * time.Second
)

// RestartCoordinator orchestrates a single graceful restart. Triggering
// it cancels the root context so daemons unblock from Run, then — after a
// bounded grace window — calls os.Exit(0) so a process supervisor
// (launchd, systemd) can respawn the binary.
//
// Only the first Request that passes the cool-down window wins; subsequent
// callers receive false and the request is recorded in the audit log.
// Safe for concurrent use.
type RestartCoordinator struct {
	cancel   context.CancelFunc
	logger   *slog.Logger
	cooldown time.Duration
	grace    time.Duration
	exit     func(int)
	now      func() time.Time

	mu        sync.Mutex
	fired     bool
	lastFired time.Time
}

// NewRestartCoordinator wires a coordinator against the given root-context
// cancel. cooldown is the minimum interval between accepted requests;
// grace is the maximum time the coordinator waits for graceful drain
// before forcing os.Exit(0). Non-positive values fall back to defaults.
func NewRestartCoordinator(cancel context.CancelFunc, logger *slog.Logger, cooldown, grace time.Duration) *RestartCoordinator {
	if logger == nil {
		logger = slog.Default()
	}
	if cooldown <= 0 {
		cooldown = defaultRestartCooldown
	}
	if grace <= 0 {
		grace = defaultRestartGrace
	}
	return &RestartCoordinator{
		cancel:   cancel,
		logger:   logger,
		cooldown: cooldown,
		grace:    grace,
		exit:     os.Exit,
		now:      time.Now,
	}
}

// Request attempts to trigger a restart. Returns true if the shutdown
// sequence has begun, false if the request was declined (already firing,
// or within the cool-down window). Both outcomes are recorded in the
// audit log with the supplied metadata.
func (c *RestartCoordinator) Request(req RestartRequest) bool {
	c.mu.Lock()
	now := c.now()
	if c.fired {
		c.mu.Unlock()
		c.logger.Info("restart request ignored: shutdown already in progress",
			"source", string(req.Source),
			"user", req.UserID,
			"channel", req.Channel,
		)
		return false
	}
	if !c.lastFired.IsZero() && now.Sub(c.lastFired) < c.cooldown {
		c.mu.Unlock()
		c.logger.Info("restart request rejected by cool-down",
			"source", string(req.Source),
			"user", req.UserID,
			"channel", req.Channel,
			"cooldown", c.cooldown.String(),
			"since_last", now.Sub(c.lastFired).String(),
		)
		return false
	}
	c.fired = true
	c.lastFired = now
	c.mu.Unlock()

	c.logger.Info("restart triggered",
		"source", string(req.Source),
		"user", req.UserID,
		"channel", req.Channel,
		"reason", req.Reason,
		"grace", c.grace.String(),
	)

	if c.cancel != nil {
		c.cancel()
	}
	go c.exitAfterGrace(req.Source)
	return true
}

func (c *RestartCoordinator) exitAfterGrace(source RestartSource) {
	time.Sleep(c.grace)
	c.logger.Info("restart grace elapsed, exiting", "source", string(source))
	c.exit(0)
}
