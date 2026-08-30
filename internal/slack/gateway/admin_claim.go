package gateway

import (
	"context"
	"strings"
	"sync"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/slack/alertcard"
)

// This file implements the first-user-wins adoption of a freshly installed
// Murtaugh.
//
// The install now ends in Slack rather than in the terminal, which creates a
// chicken-and-egg problem: every notification path here begins by asking who
// the admin is, and on a fresh install nobody has said. The daemon would come
// up correctly configured and completely silent.
//
// So an unclaimed daemon adopts the first person who direct-messages it. That
// is trust-on-first-use, and it is a real trade: whoever finds the bot first
// becomes its administrator. It is defensible only because of when it happens —
// a bot nobody has been told about, in the minutes between installing it and
// configuring it. It is a one-time door: the moment an admin exists the claim
// path is closed for good, and the adoption is announced and logged so a
// surprise is at least a visible one.
//
// It is deliberately NOT gated on the allowlist, because on a fresh install the
// allowlist is empty and gating on it would close the only door in.

// AdminClaimer persists the adopted admin so the decision outlives the process.
// The composition root supplies a closure over the config store, keeping this
// package free of any dependency on it.
type AdminClaimer func(ctx context.Context, userID string) error

// WithAdminClaimer enables first-user-wins adoption. Without one the gateway
// still adopts an admin in memory for this process, but the choice is forgotten
// on restart — which is the right degradation for a CLI/MCP build or a test,
// and never happens in the daemon.
func (a *Gateway) WithAdminClaimer(claim AdminClaimer) *Gateway {
	a.claimAdmin = claim
	return a
}

// adminClaimMu serialises the claim so two DMs arriving together cannot both
// win. Only the leader serves, so one process-local mutex is the whole of the
// contention: a standby is not reading DMs at all.
var adminClaimMu sync.Mutex

// unclaimed reports whether this daemon has no administrator yet.
func (a *Gateway) unclaimed() bool {
	return strings.TrimSpace(a.access().AdminUser) == ""
}

// claimAdminUser adopts userID as the administrator, reporting whether it took.
//
// The in-memory config is updated first so everything downstream — the
// allowlist check, every DM path, the App Home panel — sees the new admin
// immediately, without waiting for a restart to re-read the store.
func (a *Gateway) claimAdminUser(ctx context.Context, userID string) bool {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}

	adminClaimMu.Lock()
	defer adminClaimMu.Unlock()
	if !a.unclaimed() {
		return false // somebody got here first
	}

	a.setAccess(func(access *config.AccessConfig) { access.AdminUser = userID })
	if a.auth != nil {
		access := a.access()
		a.auth.SetAdmin(access.AdminUser, a.access().IsAdminUser)
	}

	if a.claimAdmin != nil {
		if err := a.claimAdmin(ctx, userID); err != nil {
			// The claim stands for this process either way; losing it on
			// restart is better than refusing the only person who can finish
			// the install.
			a.logger.Error("could not persist the administrator; it will be re-claimed after a restart",
				"user", userID, "error", err)
		}
	}
	a.logger.Warn("adopted the first user to make contact as administrator", "user", userID)
	return true
}

// handleAdminClaim adopts the sender and welcomes them. It reports whether the
// message was consumed as a claim, so the caller knows not to treat it as chat.
func (a *Gateway) handleAdminClaim(ctx context.Context, userID, channelID string) bool {
	if !a.unclaimed() {
		return false
	}
	if !a.claimAdminUser(ctx, userID) {
		return false
	}
	if _, _, err := a.postLifecycleAlert(ctx, channelID, "", adminClaimedAlert(userID)); err != nil {
		a.logger.Warn("could not confirm the administrator claim", "error", err)
	}

	// Offer the setup form straight away. The zero-agent prompt otherwise fires
	// only on promotion, which has already happened by the time anybody can DM
	// — so a fresh install would greet its new administrator and then go
	// silent until the next restart, which is the opposite of finishing the
	// install in Slack.
	if len(a.agentProfiles) == 0 {
		a.NotifyNoAgents(ctx)
	}
	return true
}

// adminClaimedAlert welcomes the new administrator.
//
// It names them explicitly rather than saying "you". This message is the only
// record that adoption happened, and an operator who later wonders how a
// particular account became the administrator should be able to read the answer
// rather than infer it.
func adminClaimedAlert(userID string) alertcard.Spec {
	return alertcard.Spec{
		Level:    alertcard.LevelInfo,
		Title:    "Murtaugh is yours",
		Subtitle: "You are the first person to make contact, so you are now the administrator.",
		Text:     "Recorded <@" + userID + "> as `access.admin_user`. Nobody else can claim this instance now.",
	}
}
