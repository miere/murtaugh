package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/agentruntime"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/credwarden"
	"github.com/miere/murtaugh/internal/slack/alertcard"
	slackclient "github.com/miere/murtaugh/internal/slack/client"
)

// cardSink records what the alerter posted and edited.
type cardSink struct {
	mu      sync.Mutex
	posts   []slackclient.PostMessageParams
	updates []slackclient.UpdateMessageParams
	postErr error
}

func (s *cardSink) PostMessage(_ context.Context, p slackclient.PostMessageParams) (slackclient.PostMessageResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.postErr != nil {
		return slackclient.PostMessageResult{}, s.postErr
	}
	s.posts = append(s.posts, p)
	return slackclient.PostMessageResult{Channel: p.ChannelID, TS: "ts-1"}, nil
}

func (s *cardSink) UpdateMessage(_ context.Context, p slackclient.UpdateMessageParams) (slackclient.PostMessageResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, p)
	return slackclient.PostMessageResult{Channel: p.ChannelID, TS: p.TS}, nil
}

func (s *cardSink) snapshot() ([]slackclient.PostMessageParams, []slackclient.UpdateMessageParams) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]slackclient.PostMessageParams(nil), s.posts...),
		append([]slackclient.UpdateMessageParams(nil), s.updates...)
}

func testAlerter(t *testing.T, sink *cardSink, admin string) *credentialAlerter {
	t.Helper()
	a := newCredentialAlerter(
		alertcard.NewRenderer("", assets.FS),
		sink, sink,
		func(_ context.Context, userID string) (string, error) { return "D-" + userID, nil },
		func() string { return admin },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if a == nil {
		t.Fatal("newCredentialAlerter returned nil with everything wired")
	}
	return a
}

var degraded = credwarden.Health{
	Identity:  credwarden.Identity{Command: "/usr/local/bin/claude"},
	Degraded:  true,
	Reason:    "read expiry: keychain: credential carries no expiresAt",
	Since:     time.Now().Add(-2 * time.Minute),
	ExpiresAt: time.Now().Add(3 * time.Minute),
}

// The alert has to reach the admin's DM, not a channel: the credential is theirs
// alone, and the failure text quotes the store's own error.
func TestCredentialAlertGoesToTheAdminDM(t *testing.T) {
	sink := &cardSink{}
	testAlerter(t, sink, "UADMIN").Observe(degraded)

	posts, _ := sink.snapshot()
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want 1", len(posts))
	}
	if posts[0].ChannelID != "D-UADMIN" {
		t.Errorf("posted to %q, want the admin DM", posts[0].ChannelID)
	}
	// The reason is the useful part — it is what the operator would otherwise
	// have had to go and grep the daemon log for.
	if body := string(posts[0].Blocks); !strings.Contains(body, "no expiresAt") {
		t.Errorf("card does not carry the failure reason:\n%s", body)
	}
	// And it has to say what to do about it, since the card carries no button.
	if body := string(posts[0].Blocks); !strings.Contains(body, "auth login") {
		t.Errorf("card does not tell the admin how to fix it:\n%s", body)
	}
}

// One card per outage. The recovery edits the outage card rather than posting a
// second message, so the admin's DM holds one artefact whose current state is
// the credential's current state.
func TestCredentialRecoveryEditsTheSameCard(t *testing.T) {
	sink := &cardSink{}
	a := testAlerter(t, sink, "UADMIN")

	a.Observe(degraded)
	a.Observe(credwarden.Health{
		Identity:  degraded.Identity,
		Since:     degraded.Since,
		ExpiresAt: time.Now().Add(8 * time.Hour),
	})

	posts, updates := sink.snapshot()
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want the outage card only", len(posts))
	}
	if len(updates) != 1 {
		t.Fatalf("got %d updates, want the outage card edited on recovery", len(updates))
	}
	if updates[0].TS != "ts-1" || updates[0].ChannelID != "D-UADMIN" {
		t.Errorf("edited %s/%s, want the card that was posted", updates[0].ChannelID, updates[0].TS)
	}
	if body := string(updates[0].Blocks); !strings.Contains(body, "recovered") {
		t.Errorf("recovery card does not say so:\n%s", body)
	}
}

// A recovery with no outage card — the outage began before this process did —
// must stay quiet. Good news is not worth an unprompted DM on its own.
func TestCredentialRecoveryWithoutAnOutageCardIsSilent(t *testing.T) {
	sink := &cardSink{}
	testAlerter(t, sink, "UADMIN").Observe(credwarden.Health{Identity: degraded.Identity})

	posts, updates := sink.snapshot()
	if len(posts) != 0 || len(updates) != 0 {
		t.Fatalf("recovery alone should say nothing, got %d posts and %d updates", len(posts), len(updates))
	}
}

// No admin configured means nobody owns the credential, so there is nobody to
// tell and the warden's log line is the whole record.
func TestCredentialAlertWithoutAnAdminIsSilent(t *testing.T) {
	sink := &cardSink{}
	testAlerter(t, sink, "   ").Observe(degraded)

	if posts, _ := sink.snapshot(); len(posts) != 0 {
		t.Fatalf("got %d posts with no admin configured", len(posts))
	}
}

// A failed post must not leave a timestamp behind: the next recovery would
// otherwise try to edit a message that does not exist.
func TestCredentialAlertForgetsAFailedPost(t *testing.T) {
	sink := &cardSink{postErr: errors.New("channel_not_found")}
	a := testAlerter(t, sink, "UADMIN")

	a.Observe(degraded)
	sink.mu.Lock()
	sink.postErr = nil
	sink.mu.Unlock()
	a.Observe(credwarden.Health{Identity: degraded.Identity})

	if _, updates := sink.snapshot(); len(updates) != 0 {
		t.Fatalf("edited a card that was never posted: %+v", updates)
	}
}

// The alerter is optional everywhere. Without a Slack client there is nothing to
// post with, and the warden must simply run without an observer.
func TestNilCredentialAlerterIsInert(t *testing.T) {
	if got := newCredentialAlerter(nil, nil, nil, nil, nil, nil); got != nil {
		t.Fatal("an unwired alerter must be nil, not live")
	}
	if got := credentialAlertObserver(nil); got != nil {
		t.Fatal("a nil alerter must yield no observer, not a nil method value")
	}
	var a *credentialAlerter
	a.Observe(degraded) // must not panic
}

// An expiry already behind us reads as lapsed rather than as a negative
// duration, because that is the difference between "renew soon" and "you are
// already locked out".
func TestRelativeExpirySaysWhenItHasLapsed(t *testing.T) {
	now := time.Now()
	if got := relativeExpiry(now.Add(-90*time.Minute), now); !strings.Contains(got, "lapsed") {
		t.Errorf("relativeExpiry = %q, want it to say the credential has lapsed", got)
	}
	if got := relativeExpiry(now.Add(90*time.Minute), now); !strings.HasPrefix(got, "in ") {
		t.Errorf("relativeExpiry = %q, want a distance from now", got)
	}
}

func nodeReport(degraded bool) agentruntime.CredentialHealth {
	h := agentruntime.CredentialHealth{
		NodeID: "node-1", Owner: "UOWNER", Credential: "/usr/local/bin/claude",
		Degraded: degraded, Since: time.Now().Add(-2 * time.Minute), ExpiresAt: time.Now().Add(3 * time.Minute),
	}
	if degraded {
		h.Reason = "read expiry: keychain: credential carries no expiresAt"
	}
	return h
}

func gatewayWithAlerts(sink *cardSink, t *testing.T) *Gateway {
	return &Gateway{
		credAlerts: testAlerter(t, sink, "UADMIN"),
		cfg:        config.AccessConfig{AdminUser: "UADMIN", AllowedUsers: []string{"UOWNER"}},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestANodesFailingCredentialIsToldToItsOwner(t *testing.T) {
	sink := &cardSink{}
	g := gatewayWithAlerts(sink, t)

	g.alertNodeCredential(nodeReport(true))
	g.alertNodeCredential(nodeReport(true))
	posts, _ := sink.snapshot()
	if len(posts) != 1 {
		t.Fatalf("got %d posts, want one card for one outage", len(posts))
	}
	if posts[0].ChannelID != "D-UOWNER" {
		t.Fatalf("posted to %q, want the node owner's DM", posts[0].ChannelID)
	}
	if body := string(posts[0].Blocks); !strings.Contains(body, "node-1") || !strings.Contains(body, "no expiresAt") {
		t.Errorf("card does not say which node or why:\n%s", body)
	}

	g.alertNodeCredential(nodeReport(false))
	_, updates := sink.snapshot()
	if len(updates) != 1 || updates[0].ChannelID != "D-UOWNER" || updates[0].TS != "ts-1" {
		t.Fatalf("recovery edited %+v, want the owner's card edited in place", updates)
	}
	if body := string(updates[0].Blocks); !strings.Contains(body, "recovered") {
		t.Errorf("recovery card does not say so:\n%s", body)
	}
}

func TestANodeOwnerWhoMayNotUseTheGatewayIsNotTold(t *testing.T) {
	sink := &cardSink{}
	g := gatewayWithAlerts(sink, t)
	report := nodeReport(true)
	report.Owner = "USTRANGER"
	g.alertNodeCredential(report)
	if posts, _ := sink.snapshot(); len(posts) != 0 {
		t.Fatalf("posted %d cards to an owner who may not use the gateway", len(posts))
	}
}

func TestACredentialThatFailsAgainSoonAfterRecoveringEditsItsCardBack(t *testing.T) {
	sink := &cardSink{}
	g := gatewayWithAlerts(sink, t)
	g.alertNodeCredential(nodeReport(true))
	g.alertNodeCredential(nodeReport(false))
	g.alertNodeCredential(nodeReport(true))

	posts, updates := sink.snapshot()
	if len(posts) != 1 {
		t.Fatalf("got %d posts for a credential that flipped back to failing, want the one card", len(posts))
	}
	if len(updates) != 2 || !strings.Contains(string(updates[1].Blocks), "is failing") {
		t.Fatalf("got %d edits; the card was not turned back to failing", len(updates))
	}
}
