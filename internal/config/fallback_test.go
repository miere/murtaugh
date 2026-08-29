package config

import (
	"testing"
	"time"
)

// TestFallbackDefaults pins the shipped timings: 30s of lease renewed every
// 10s, standing down at 20s. The relationship between the three is what makes
// the scheme safe, so it is asserted as a relationship rather than as literals.
func TestFallbackDefaults(t *testing.T) {
	f := FallbackConfig{Enabled: true}

	lease, renew, demote := f.EffectiveLease(), f.EffectiveRenew(), f.EffectiveDemoteAfter()
	if lease != 30*time.Second {
		t.Errorf("EffectiveLease() = %v, want 30s", lease)
	}
	if renew != 10*time.Second {
		t.Errorf("EffectiveRenew() = %v, want 10s", renew)
	}
	if demote >= lease {
		t.Errorf("demote-after (%v) must be strictly less than the lease (%v), or an outgoing leader overlaps its successor", demote, lease)
	}
	if renew*2 > lease {
		t.Errorf("renew (%v) leaves fewer than two attempts inside the lease (%v); leadership would flap on one lost packet", renew, lease)
	}
}

// TestFallbackDemoteAfterLeavesRoomForARetry checks the gap between standing
// down and the lease lapsing is at least one renewal interval, across a range of
// configured values — that gap is the safety margin, not an incidental result of
// the default numbers.
func TestFallbackDemoteAfterLeavesRoomForARetry(t *testing.T) {
	for _, tc := range []struct{ lease, renew int }{
		{30, 10}, {60, 20}, {20, 10}, {120, 15}, {0, 0},
	} {
		f := FallbackConfig{Enabled: true, LeaseSeconds: tc.lease, RenewSeconds: tc.renew}
		if err := f.Validate(); err != nil {
			t.Fatalf("lease=%d renew=%d rejected: %v", tc.lease, tc.renew, err)
		}
		gap := f.EffectiveLease() - f.EffectiveDemoteAfter()
		if gap < f.EffectiveRenew() {
			t.Errorf("lease=%d renew=%d: only %v between standing down and expiry, want at least one renewal interval (%v)",
				tc.lease, tc.renew, gap, f.EffectiveRenew())
		}
	}
}

// TestFallbackValidate covers the edits that must be refused at the store's
// write boundary rather than discovered during a failover.
func TestFallbackValidate(t *testing.T) {
	if err := (FallbackConfig{}).Validate(); err != nil {
		t.Errorf("a disabled fallback block was rejected: %v", err)
	}
	// Timings are not checked while disabled: there is no election to break.
	if err := (FallbackConfig{LeaseSeconds: 1, RenewSeconds: 900}).Validate(); err != nil {
		t.Errorf("nonsense timings rejected while disabled: %v", err)
	}

	for name, f := range map[string]FallbackConfig{
		"renew exceeds lease":    {Enabled: true, LeaseSeconds: 10, RenewSeconds: 20},
		"renew is half plus one": {Enabled: true, LeaseSeconds: 10, RenewSeconds: 6},
		"negative lease":         {Enabled: true, LeaseSeconds: -1},
		"negative renew":         {Enabled: true, RenewSeconds: -5},
	} {
		t.Run(name, func(t *testing.T) {
			if err := f.Validate(); err == nil {
				t.Error("Validate accepted timings that would flap or never renew")
			}
		})
	}
}

// TestConfigValidateRejectsBadFallback confirms the block is wired into the
// single validated core, so a bad `cfg fallback set` is rolled back rather than
// persisted.
func TestConfigValidateRejectsBadFallback(t *testing.T) {
	base := Config{
		OAuth:    OAuthConfig{AppToken: "xapp-x", BotToken: "xoxb-x"},
		Fallback: FallbackConfig{Enabled: true, LeaseSeconds: 5, RenewSeconds: 30},
	}
	if err := base.Validate(); err == nil {
		t.Fatal("Config.Validate accepted a fallback block that Validate rejects on its own")
	}

	base.Fallback = FallbackConfig{Enabled: true}
	if err := base.Validate(); err != nil {
		t.Errorf("Config.Validate rejected a default fallback block: %v", err)
	}
}
