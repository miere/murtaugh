package gateway

import (
	"context"
)

// StartBackground starts the work that must run for the DAEMON's lifetime,
// independent of leadership.
//
// The credential warden belongs here rather than in StartServing, and the
// distinction is not bookkeeping. A Claude Code credential is scoped to the
// MACHINE — one keychain item per user, shared by every claude_code agent on the
// host — while leadership is scoped to the cluster. Tying the warden to
// leadership means a standby node lets its own credential rot, so the failover
// that promotes it hands service to a node that cannot authenticate. That is the
// opposite of what a standby is for.
//
// It is also how a real lockout happened: the daemon stood down at 08:10 and the
// warden stopped with it, leaving the credential unattended for twenty-five
// hours. Nothing about a node's leadership makes its credential need refreshing
// any less.
//
// Idempotent: a second call while already running is ignored. A nil warden (no
// claude_code agent configured) is a no-op.
func (a *Gateway) StartBackground(ctx context.Context) {
	if a == nil || a.credWarden == nil {
		return
	}
	a.backgroundMu.Lock()
	defer a.backgroundMu.Unlock()
	if a.backgroundCancel != nil {
		return
	}
	bgCtx, cancel := context.WithCancel(ctx)
	a.backgroundCancel = cancel
	go a.credWarden.Run(bgCtx)
}

// StopBackground stops the daemon-lifetime work. It is called when a gateway is
// replaced by a configuration reload, so the outgoing instance does not leave a
// second warden running against the same credential — two of them would race the
// server's rotation of the refresh token, which is the failure the warden exists
// to prevent. Safe to call more than once, and on a gateway that never started.
func (a *Gateway) StopBackground() {
	if a == nil {
		return
	}
	a.backgroundMu.Lock()
	cancel := a.backgroundCancel
	a.backgroundCancel = nil
	a.backgroundMu.Unlock()
	if cancel != nil {
		cancel()
	}
}
