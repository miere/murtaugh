package gateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

// claimGateway builds a gateway in the unclaimed state, with a recording
// claimer standing in for the config store.
func claimGateway(t *testing.T, api *recordingCardAPI) (*Gateway, *[]string) {
	t.Helper()
	var claimed []string
	var mu sync.Mutex
	gw := &Gateway{
		logger:      quietLogger(),
		alertAPI:    api,
		alertEditor: api,
		messaging:   stubMessaging{},
		alertCards:  testAlertCards(),
	}
	gw.WithAdminClaimer(func(_ context.Context, userID string) error {
		mu.Lock()
		defer mu.Unlock()
		claimed = append(claimed, userID)
		return nil
	})
	return gw, &claimed
}

// TestFirstDMClaimsTheInstance is the onboarding entry point: a freshly
// installed daemon has no admin, so every notification path is silent until
// somebody adopts it.
func TestFirstDMClaimsTheInstance(t *testing.T) {
	api := &recordingCardAPI{}
	gw, claimed := claimGateway(t, api)
	ctx := context.Background()

	if !gw.unclaimed() {
		t.Fatal("a gateway with no admin_user did not report as unclaimed")
	}
	if !gw.handleAdminClaim(ctx, "U01FIRST", "D01FIRST") {
		t.Fatal("the first DM did not claim the instance")
	}

	if gw.cfg.AdminUser != "U01FIRST" {
		t.Errorf("admin_user = %q, want U01FIRST", gw.cfg.AdminUser)
	}
	if !gw.cfg.IsAdminUser("U01FIRST") {
		t.Error("the claimant is not recognised as admin; every admin-gated control would still refuse them")
	}
	if !gw.cfg.IsAllowedUser("U01FIRST") {
		t.Error("the claimant is not allowed; their next message would be ignored")
	}
	if len(*claimed) != 1 || (*claimed)[0] != "U01FIRST" {
		t.Errorf("persisted claims = %v, want [U01FIRST]", *claimed)
	}
	if posts, _ := api.snapshot(); len(posts) != 1 {
		t.Errorf("%d confirmations posted, want 1", len(posts))
	}
}

// TestClaimIsOneTime is the security property. The window is defensible only
// because it shuts: once an administrator exists, a second person must not be
// able to take the instance from them.
func TestClaimIsOneTime(t *testing.T) {
	api := &recordingCardAPI{}
	gw, claimed := claimGateway(t, api)
	ctx := context.Background()

	if !gw.handleAdminClaim(ctx, "U01FIRST", "D01FIRST") {
		t.Fatal("the first claim failed")
	}
	if gw.handleAdminClaim(ctx, "U01SECOND", "D01SECOND") {
		t.Fatal("a second user took the instance from its administrator")
	}
	if gw.cfg.AdminUser != "U01FIRST" {
		t.Errorf("admin_user = %q after a second attempt, want U01FIRST", gw.cfg.AdminUser)
	}
	if len(*claimed) != 1 {
		t.Errorf("%d claims persisted, want 1", len(*claimed))
	}
}

// TestConcurrentClaimsElectOne covers two people DMing at once during the
// install window — unlikely, but the whole feature is a race by construction.
func TestConcurrentClaimsElectOne(t *testing.T) {
	api := &recordingCardAPI{}
	gw, claimed := claimGateway(t, api)
	ctx := context.Background()

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
		start   = make(chan struct{})
	)
	for _, user := range []string{"U01AAA", "U01BBB", "U01CCC", "U01DDD"} {
		wg.Add(1)
		go func(user string) {
			defer wg.Done()
			<-start
			if gw.handleAdminClaim(ctx, user, "D01"+user) {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}(user)
	}
	close(start)
	wg.Wait()

	if winners != 1 {
		t.Fatalf("%d users claimed the instance, want exactly 1", winners)
	}
	if len(*claimed) != 1 {
		t.Errorf("%d claims persisted, want 1", len(*claimed))
	}
}

// TestAlreadyClaimedGatewayIgnoresTheClaimPath pins that a configured daemon
// never enters this code at all — the door is shut before anyone knocks.
func TestAlreadyClaimedGatewayIgnoresTheClaimPath(t *testing.T) {
	api := &recordingCardAPI{}
	gw, claimed := claimGateway(t, api)
	gw.cfg = config.AccessConfig{AdminUser: "U01OWNER"}

	if gw.unclaimed() {
		t.Fatal("a configured gateway reported as unclaimed")
	}
	if gw.handleAdminClaim(context.Background(), "U01GUEST", "D01GUEST") {
		t.Fatal("a configured gateway accepted a claim")
	}
	if len(*claimed) != 0 {
		t.Errorf("%d claims persisted against a configured gateway", len(*claimed))
	}
}

// TestClaimSurvivesAFailedWrite keeps the install completable when the store
// refuses: losing the choice on restart beats refusing the only person who can
// finish setting the daemon up.
func TestClaimSurvivesAFailedWrite(t *testing.T) {
	api := &recordingCardAPI{}
	gw := &Gateway{
		logger:      quietLogger(),
		alertAPI:    api,
		alertEditor: api,
		messaging:   stubMessaging{},
		alertCards:  testAlertCards(),
	}
	gw.WithAdminClaimer(func(context.Context, string) error { return errors.New("store unreachable") })

	if !gw.handleAdminClaim(context.Background(), "U01FIRST", "D01FIRST") {
		t.Fatal("a failed persist blocked the claim")
	}
	if gw.cfg.AdminUser != "U01FIRST" {
		t.Errorf("admin_user = %q, want the in-memory claim to stand", gw.cfg.AdminUser)
	}
}

// TestClaimConfirmationNamesTheUser checks the adoption leaves a record. This
// message is the only trace of how a given account became the administrator.
func TestClaimConfirmationNamesTheUser(t *testing.T) {
	spec := adminClaimedAlert("U01FIRST")
	if !strings.Contains(spec.Text, "U01FIRST") {
		t.Errorf("the confirmation does not name the new admin: %q", spec.Text)
	}
	if !strings.Contains(strings.ToLower(spec.Text), "nobody else") {
		t.Errorf("the confirmation does not say the door has shut: %q", spec.Text)
	}
}
