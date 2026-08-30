package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/slack-go/slack"

	slackclient "github.com/miere/murtaugh/internal/slack/client"
	"github.com/miere/murtaugh/internal/slack/configcard"
)

// This file owns the configuration-change approval conversation: post the diff
// to the admin, block until they decide, and record the outcome on the card.
//
// It is a bespoke flow rather than a call into the `ask` tool's broker. `ask`
// renders a question and a row of options; this renders a diff, states a
// consequence, and drives a decision that mutates the config store whichever
// way it goes. Folding it into `ask` would teach that tool about diffs and
// rollbacks it has no other reason to know, and every future `ask` caller would
// carry the weight.

// ConfigDecision is what the admin chose.
type ConfigDecision int

const (
	// ConfigRollback restores the running configuration over the edit.
	ConfigRollback ConfigDecision = iota
	// ConfigApply adopts the edit and reloads.
	ConfigApply
)

// errNoApprovalSurface reports that there is nowhere to ask. It is a refusal to
// guess, not a failure to render: with no admin DM available the only safe
// reading of an unreviewed change is that it has not been approved.
var errNoApprovalSurface = errors.New("no admin conversation is available to approve a configuration change")

// pendingConfigChange is one open decision awaiting a click.
//
// channel/ts are guarded because the decision is registered before the card is
// posted (see RequestConfigApproval), so a click can be reading them on the
// router's goroutine while the post fills them in on the caller's.
type pendingConfigChange struct {
	decision chan ConfigDecision
	diff     string
	once     sync.Once

	mu      sync.Mutex
	channel string
	ts      string
}

// recordMessage stores where the card landed.
func (p *pendingConfigChange) recordMessage(channel, ts string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if channel != "" {
		p.channel = channel
	}
	p.ts = ts
}

// message returns where the card landed, if it has been posted yet.
func (p *pendingConfigChange) message() (channel, ts string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.channel, p.ts
}

// settle delivers the decision exactly once, so a double-click — or a click
// racing the context deadline — cannot panic on a closed channel or leave a
// second waiter hanging.
func (p *pendingConfigChange) settle(d ConfigDecision) bool {
	delivered := false
	p.once.Do(func() {
		p.decision <- d
		delivered = true
	})
	return delivered
}

// RequestConfigApproval posts the diff to the admin and blocks until they
// decide or ctx ends.
//
// A context that ends first means "not approved": the caller keeps running the
// configuration it already has. That is the only safe default — adopting an
// unreviewed change because nobody answered would make the review theatre.
func (a *Gateway) RequestConfigApproval(ctx context.Context, diff string) (ConfigDecision, error) {
	if strings.TrimSpace(diff) == "" {
		return ConfigRollback, errors.New("refusing to ask about an empty configuration diff")
	}

	dest, err := a.resolveSuggestionDestination(ctx, "")
	if err != nil {
		return ConfigRollback, fmt.Errorf("%w: %v", errNoApprovalSurface, err)
	}
	if dest == "" {
		return ConfigRollback, errNoApprovalSurface
	}

	corr := uuid.NewString()
	pending := &pendingConfigChange{decision: make(chan ConfigDecision, 1), channel: dest, diff: diff}

	blocks, err := a.configCards.Pending(corr, diff)
	if err != nil {
		return ConfigRollback, err
	}

	// Register BEFORE posting. The card carries clickable action_ids the moment
	// Slack renders it, and an admin who is already looking at the DM can press
	// a button before this function reaches its next statement. Registering
	// afterwards leaves a window in which a real click finds no pending
	// decision and is dropped — the button appears dead, and the change sits
	// unreviewed until the request expires.
	a.registerConfigChange(corr, pending)
	defer a.unregisterConfigChange(corr)

	res, err := a.postRawCard(ctx, dest, blocks, configcard.PlainText(diff))
	if err != nil {
		return ConfigRollback, fmt.Errorf("post configuration approval: %w", err)
	}
	// The message coordinates are only needed to edit the card afterwards, so
	// filling them in after the post is fine — a click that beat us here still
	// found the decision, and settleConfigCard tolerates an empty ts.
	pending.recordMessage(res.Channel, res.TS)

	select {
	case decision := <-pending.decision:
		return decision, nil
	case <-ctx.Done():
		// Leave a record rather than a live-looking prompt nobody can act on.
		a.settleConfigCard(context.WithoutCancel(ctx), pending,
			"No answer before the request expired; the previous configuration was kept.")
		return ConfigRollback, ctx.Err()
	}
}

// postRawCard posts pre-rendered Block Kit, degrading to text when no
// raw-blocks client is wired — the same fallback every other card here takes.
func (a *Gateway) postRawCard(ctx context.Context, channel string, blocks []byte, fallback string) (slackclient.PostMessageResult, error) {
	if a.alertAPI != nil {
		return a.alertAPI.PostMessage(ctx, slackclient.PostMessageParams{
			ChannelID: channel,
			Text:      fallback,
			Blocks:    blocks,
		})
	}
	if a.messaging == nil {
		return slackclient.PostMessageResult{}, errNoSlackMessaging
	}
	ch, ts, err := a.messaging.PostMessageContext(ctx, channel, slack.MsgOptionText(fallback, false))
	return slackclient.PostMessageResult{Channel: ch, TS: ts}, err
}

// settleConfigCard rewrites the card with its outcome, removing the buttons.
func (a *Gateway) settleConfigCard(ctx context.Context, pending *pendingConfigChange, footer string) {
	channel, ts := pending.message()
	if a.alertEditor == nil || a.configCards == nil || ts == "" {
		// Not posted yet (a click that beat the post) or no editor wired. The
		// decision still stands; only the card's record of it is missing.
		return
	}
	blocks, err := a.configCards.Settled(pending.diff, footer)
	if err != nil {
		a.logger.Warn("could not render the settled configuration card", "error", err)
		return
	}
	if _, err := a.alertEditor.UpdateMessage(ctx, slackclient.UpdateMessageParams{
		ChannelID: channel,
		TS:        ts,
		Text:      footer,
		Blocks:    blocks,
	}); err != nil {
		a.logger.Warn("could not update the configuration card", "error", err)
	}
}

// isConfigApprovalInteraction reports whether a callback belongs to this card.
func isConfigApprovalInteraction(interaction slack.InteractionCallback) bool {
	for _, action := range interaction.ActionCallback.BlockActions {
		if _, _, ok := configcard.ParseActionID(action.ActionID); ok {
			return true
		}
	}
	return false
}

// handleConfigApprovalClick routes a button press back to the waiting decision.
//
// It re-checks IsAdminUser rather than trusting the router, as the App Home
// update and restart controls do. The router admits any allowlisted user to
// built-ins, and that is not enough here: approving adopts whatever is
// currently in the store, which may be an edit that widens the allowlist
// itself. An allowlisted user must not be able to promote themselves by
// clicking a button meant for the admin.
func (a *Gateway) handleConfigApprovalClick(ctx context.Context, interaction slack.InteractionCallback) {
	if !a.access().IsAdminUser(interaction.User.ID) {
		a.logger.Warn("ignoring a configuration approval click from a non-admin", "user", interaction.User.ID)
		return
	}
	for _, action := range interaction.ActionCallback.BlockActions {
		corr, choice, ok := configcard.ParseActionID(action.ActionID)
		if !ok {
			continue
		}
		pending, found := a.configChange(corr)
		if !found {
			// The daemon restarted, or the decision already settled. Say so on
			// the card rather than leaving a button that silently does nothing.
			a.logger.Info("configuration approval click has no pending decision", "corr", corr)
			continue
		}

		decision := ConfigRollback
		footer := fmt.Sprintf("Rolled back by <@%s>; the running configuration was restored.", interaction.User.ID)
		if choice == configcard.ActionApply {
			decision = ConfigApply
			footer = fmt.Sprintf("Approved by <@%s>; reloading the configuration.", interaction.User.ID)
		}

		// Settle the card before delivering the decision. The reload that
		// follows an approval tears down and rebuilds this gateway, so anything
		// left until afterwards may be running on a different object — or not
		// running at all.
		a.settleConfigCard(ctx, pending, footer)
		if !pending.settle(decision) {
			a.logger.Info("configuration decision was already settled", "corr", corr)
		}
		return
	}
}

// registerConfigChange records a pending decision.
func (a *Gateway) registerConfigChange(corr string, pending *pendingConfigChange) {
	a.configChangesMu.Lock()
	defer a.configChangesMu.Unlock()
	if a.configChanges == nil {
		a.configChanges = make(map[string]*pendingConfigChange)
	}
	a.configChanges[corr] = pending
}

func (a *Gateway) unregisterConfigChange(corr string) {
	a.configChangesMu.Lock()
	defer a.configChangesMu.Unlock()
	delete(a.configChanges, corr)
}

func (a *Gateway) configChange(corr string) (*pendingConfigChange, bool) {
	a.configChangesMu.Lock()
	defer a.configChangesMu.Unlock()
	pending, ok := a.configChanges[corr]
	return pending, ok
}

// NotifyConfigReloading tells the admin the approved change is being applied,
// and returns where it said so.
//
// It mirrors the restart notice deliberately: from the admin's side a soft
// reload and a restart feel the same — the bot goes quiet, agents drop their
// work, and it comes back — so it should read the same rather than making them
// learn a second vocabulary for the same experience.
//
// The returned coordinates let the completion EDIT this message rather than
// post a second one. Two blocks saying "reloading" then "reloaded" directly
// under the approval card that caused them is three messages for one event.
func (a *Gateway) NotifyConfigReloading(ctx context.Context) (channel, ts string) {
	admin := strings.TrimSpace(a.access().AdminUser)
	if admin == "" {
		return "", ""
	}
	channel, ts, err := a.postLifecycleAlert(ctx, admin, "", configReloadingAlert())
	if err != nil {
		a.logger.Warn("could not post the configuration reload notice", "error", err)
		return "", ""
	}
	return channel, ts
}

// NotifyConfigReloaded confirms the new configuration is live, replacing the
// "reloading" notice in place when one was posted.
func (a *Gateway) NotifyConfigReloaded(ctx context.Context, channel, ts string) {
	if channel != "" && ts != "" {
		if err := a.updateLifecycleAlert(ctx, channel, ts, configReloadedAlert()); err == nil {
			return
		}
		// The edit failed — the message may have been deleted, or this is a
		// rebuilt gateway with no editor wired. Fall through and post, because
		// silence after "reloading…" reads as a hung daemon.
		a.logger.Warn("could not update the configuration reload notice; posting instead")
	}
	admin := strings.TrimSpace(a.access().AdminUser)
	if admin == "" {
		return
	}
	if _, _, err := a.postLifecycleAlert(ctx, admin, "", configReloadedAlert()); err != nil {
		a.logger.Warn("could not post the configuration reloaded notice", "error", err)
	}
}
