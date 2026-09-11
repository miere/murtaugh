package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/credwarden"
	"github.com/miere/murtaugh/internal/slack/alertcard"
)

// credAlertTimeout bounds one card post. The observer runs on the warden's own
// goroutine, so an unbounded Slack call would hold up the next credential pass.
const credAlertTimeout = 10 * time.Second

type credentialAlerter struct {
	cards  *alertcard.Renderer
	poster alertMessagePoster
	editor alertMessageEditor
	dm     func(ctx context.Context, userID string) (string, error)
	admin  func() string
	logger *slog.Logger

	mu   sync.Mutex
	open map[string]postedAlert
}

type postedAlert struct {
	channel     string
	ts          string
	recoveredAt time.Time
}

var credentialFlipWindow = time.Hour

type credentialAlert struct {
	subtitle  string
	degraded  bool
	reason    string
	since     time.Time
	expiresAt time.Time
	nextSteps string
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
		open: make(map[string]postedAlert),
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
		return
	}
	c.alert(admin, h.Identity.String(), credentialAlert{
		subtitle:  h.Identity.String(),
		degraded:  h.Degraded,
		reason:    h.Reason,
		since:     h.Since,
		expiresAt: h.ExpiresAt,
		nextSteps: "Run `/murtaugh auth login` to re-authenticate. " +
			"`/murtaugh auth status` shows what the warden currently sees.",
	})
}

func (c *credentialAlerter) alertNode(h agentruntime.CredentialHealth) {
	if c == nil {
		return
	}
	c.alert(h.Owner, h.NodeID+"\x00"+h.Credential, credentialAlert{
		subtitle:  "node " + h.NodeID + ": " + h.Credential,
		degraded:  h.Degraded,
		reason:    h.Reason,
		since:     h.Since,
		expiresAt: h.ExpiresAt,
		nextSteps: "The node asks you to sign in again by DM when one of its agents is refused. To do it now, run " +
			"`/murtaugh auth login " + h.NodeID + "`, or `claude auth login` on the machine.",
	})
}

func (c *credentialAlerter) alert(recipient, key string, a credentialAlert) {
	ctx, cancel := context.WithTimeout(context.Background(), credAlertTimeout)
	defer cancel()

	c.mu.Lock()
	defer c.mu.Unlock()

	for k, p := range c.open {
		if !p.recoveredAt.IsZero() && time.Since(p.recoveredAt) >= credentialFlipWindow {
			delete(c.open, k)
		}
	}
	posted, known := c.open[key]
	failing := known && posted.recoveredAt.IsZero()
	if !a.degraded {
		if !failing || c.editor == nil {
			return
		}
		if err := updateAlertCard(ctx, c.editor, c.cards, posted.channel, posted.ts, recoveredCredentialSpec(a)); err != nil {
			c.logger.Warn("could not update the credential alert", "error", err)
			return
		}
		posted.recoveredAt = time.Now()
		c.open[key] = posted
		return
	}
	if failing {
		return
	}
	if known && c.editor != nil {
		if err := updateAlertCard(ctx, c.editor, c.cards, posted.channel, posted.ts, degradedCredentialSpec(a)); err != nil {
			c.logger.Warn("could not update the credential alert", "error", err)
			return
		}
		posted.recoveredAt = time.Time{}
		c.open[key] = posted
		return
	}
	channel, err := c.dm(ctx, recipient)
	if err != nil {
		c.logger.Warn("could not open a DM for a credential alert", "user", recipient, "error", err)
		return
	}
	res, err := postAlertCard(ctx, c.poster, c.cards, channel, "", degradedCredentialSpec(a))
	if err != nil {
		c.logger.Warn("could not post the credential alert", "error", err)
		return
	}
	c.open[key] = postedAlert{channel: res.Channel, ts: res.TS}
}

func degradedCredentialSpec(a credentialAlert) alertcard.Spec {
	spec := alertcard.Spec{
		Level:     alertcard.LevelError,
		Title:     "Claude Code credential is failing",
		Subtitle:  a.subtitle,
		Reason:    a.reason,
		NextSteps: a.nextSteps,
	}
	if !a.expiresAt.IsZero() {
		spec.Text = fmt.Sprintf("Last observed expiry: %s.", relativeExpiry(a.expiresAt, time.Now()))
	} else {
		spec.Text = "The warden has never been able to read this credential's expiry."
	}
	return spec
}

func recoveredCredentialSpec(a credentialAlert) alertcard.Spec {
	spec := alertcard.Spec{
		Level:    alertcard.LevelNotice,
		Title:    "Claude Code credential recovered",
		Subtitle: a.subtitle,
	}
	if !a.since.IsZero() {
		spec.Text = fmt.Sprintf("It was failing for %s.", time.Since(a.since).Round(time.Second))
	}
	if !a.expiresAt.IsZero() {
		spec.NextSteps = fmt.Sprintf("Expiry: %s.", relativeExpiry(a.expiresAt, time.Now()))
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
