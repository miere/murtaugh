package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/miere/murtaugh/internal/credwarden"
	"github.com/miere/murtaugh/internal/slack/alertcard"
)

// credAlertTimeout bounds one card post. The observer runs on the warden's own
// goroutine, so an unbounded Slack call would hold up the next credential pass.
const credAlertTimeout = 10 * time.Second

// credentialAlerter turns the credential warden's health transitions into a
// single card in the admin's DM.
//
// It exists because on 2026-09-07 the warden logged `credential warden could not
// read expiry` every thirty seconds for thirty-eight minutes and told nobody.
// There was a way to PULL the state — the `auth status` verb — and no way for it
// to be PUSHED, so the first symptom the admin got was an agent that would not
// answer. The card carries what the status verb would have shown, unprompted.
//
// One card per outage, not one per failed pass: the degraded transition posts,
// the recovery edits that same message into a resolved notice. That is what
// keeps an alert from becoming the seventy-six DMs a level-triggered version
// would have sent.
type credentialAlerter struct {
	cards  *alertcard.Renderer
	poster alertMessagePoster
	editor alertMessageEditor
	dm     func(ctx context.Context, userID string) (string, error)
	admin  func() string
	logger *slog.Logger

	// mu serialises the post/edit pair. The warden calls the observer from one
	// goroutine, but a configuration reload can replace the gateway underneath
	// it, and two writers racing on the same TS would leave the card showing
	// whichever landed last rather than whichever happened last.
	mu      sync.Mutex
	channel string
	ts      string
}

// newCredentialAlerter returns nil when anything it needs is missing — no
// renderer, no client, no admin to tell. A nil alerter is inert, and the warden
// simply runs without an observer.
func newCredentialAlerter(
	cards *alertcard.Renderer,
	poster alertMessagePoster,
	editor alertMessageEditor,
	dm func(ctx context.Context, userID string) (string, error),
	admin func() string,
	logger *slog.Logger,
) *credentialAlerter {
	if cards == nil || poster == nil || dm == nil || admin == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &credentialAlerter{
		cards: cards, poster: poster, editor: editor,
		dm: dm, admin: admin, logger: logger,
	}
}

// Observe is the credwarden.Observer. It is called only on a crossing between
// working and not working, never on every failed pass.
func (c *credentialAlerter) Observe(h credwarden.Health) {
	if c == nil {
		return
	}
	admin := strings.TrimSpace(c.admin())
	if admin == "" {
		// Nobody owns the credential in this configuration, so there is nobody
		// to tell. The warden's log line is the whole record.
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), credAlertTimeout)
	defer cancel()

	channel, err := c.dm(ctx, admin)
	if err != nil {
		c.logger.Warn("could not open the admin DM for a credential alert", "error", err)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if !h.Degraded {
		c.resolve(ctx, h)
		return
	}
	res, err := postAlertCard(ctx, c.poster, c.cards, channel, "", degradedCredentialSpec(h))
	if err != nil {
		c.logger.Warn("could not post the credential alert", "error", err)
		return
	}
	c.channel, c.ts = res.Channel, res.TS
}

// resolve edits the outage card into its recovered form.
//
// Editing rather than posting is deliberate: a second message would mean the
// admin's DM accumulates a pair per outage, and the useful artefact is one card
// whose current state is the credential's current state.
func (c *credentialAlerter) resolve(ctx context.Context, h credwarden.Health) {
	if c.editor == nil || c.channel == "" || c.ts == "" {
		// Nothing was posted — the outage started before this gateway did, or the
		// post failed. Recovery is good news, so there is nothing to announce on
		// its own.
		return
	}
	if err := updateAlertCard(ctx, c.editor, c.cards, c.channel, c.ts, recoveredCredentialSpec(h)); err != nil {
		c.logger.Warn("could not update the credential alert", "error", err)
		return
	}
	c.channel, c.ts = "", ""
}

// degradedCredentialSpec renders the outage.
//
// NextSteps names the verb rather than offering a button because alertcard is a
// text surface with no actions; a one-line instruction the admin can act on
// beats growing an interactive surface for it.
func degradedCredentialSpec(h credwarden.Health) alertcard.Spec {
	spec := alertcard.Spec{
		Level:    alertcard.LevelError,
		Title:    "Claude Code credential is failing",
		Subtitle: h.Identity.String(),
		Reason:   h.Reason,
		NextSteps: "Run `/murtaugh auth login` to re-authenticate. " +
			"`/murtaugh auth status` shows what the warden currently sees.",
	}
	if !h.ExpiresAt.IsZero() {
		spec.Text = fmt.Sprintf("Last observed expiry: %s.", relativeExpiry(h.ExpiresAt, time.Now()))
	} else {
		// A credential the warden has never managed to read is a different
		// problem from one that is merely lapsing, and the admin should not have
		// to infer which from a missing line.
		spec.Text = "The warden has never been able to read this credential's expiry."
	}
	return spec
}

// recoveredCredentialSpec renders the same card once the credential is working
// again, carrying how long the outage lasted.
func recoveredCredentialSpec(h credwarden.Health) alertcard.Spec {
	spec := alertcard.Spec{
		Level:    alertcard.LevelNotice,
		Title:    "Claude Code credential recovered",
		Subtitle: h.Identity.String(),
	}
	if !h.Since.IsZero() {
		spec.Text = fmt.Sprintf("It was failing for %s.", time.Since(h.Since).Round(time.Second))
	}
	if !h.ExpiresAt.IsZero() {
		spec.NextSteps = fmt.Sprintf("Expiry: %s.", relativeExpiry(h.ExpiresAt, time.Now()))
	}
	return spec
}

// relativeExpiry renders an expiry the way an operator reads one: as a distance
// from now, saying plainly when it is already behind us.
func relativeExpiry(expiry, now time.Time) string {
	d := expiry.Sub(now).Round(time.Minute)
	if d < 0 {
		return (-d).String() + " ago (lapsed)"
	}
	return "in " + d.String()
}

// credentialAlertObserver adapts the alerter to the warden's Observer type,
// returning nil when there is no alerter so the warden stays observer-free
// rather than calling into a nil method value.
func credentialAlertObserver(a *credentialAlerter) credwarden.Observer {
	if a == nil {
		return nil
	}
	return a.Observe
}
