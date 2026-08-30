package config

import (
	"testing"
	"time"
)

// TestElectionDefaults pins the shipped timings: 30s of lease renewed every
// 10s, standing down at 20s. The relationship between the three is what makes
// the scheme safe, so it is asserted as a relationship rather than as literals.
func TestElectionDefaults(t *testing.T) {
	f := ElectionConfig{}

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

// TestElectionDemoteAfterLeavesRoomForARetry checks the gap between standing
// down and the lease lapsing is at least one renewal interval, across a range of
// configured values — that gap is the safety margin, not an incidental result of
// the default numbers.
func TestElectionDemoteAfterLeavesRoomForARetry(t *testing.T) {
	for _, tc := range []struct{ lease, renew int }{
		{30, 10}, {60, 20}, {20, 10}, {120, 15}, {0, 0},
	} {
		f := ElectionConfig{LeaseSeconds: tc.lease, RenewSeconds: tc.renew}
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

// TestElectionValidate covers the edits that must be refused at the store's
// write boundary rather than discovered during a failover.
func TestElectionValidate(t *testing.T) {
	// An empty block is valid and means "take the defaults" — which is what
	// every deployment that never touches these numbers gets. Election is not
	// opt-in, so there is no disabled state in which bad timings could hide.
	if err := (ElectionConfig{}).Validate(); err != nil {
		t.Errorf("an unconfigured election block was rejected: %v", err)
	}

	for name, f := range map[string]ElectionConfig{
		"renew exceeds lease":    {LeaseSeconds: 10, RenewSeconds: 20},
		"renew is half plus one": {LeaseSeconds: 10, RenewSeconds: 6},
		"negative lease":         {LeaseSeconds: -1},
		"negative renew":         {RenewSeconds: -5},
	} {
		t.Run(name, func(t *testing.T) {
			if err := f.Validate(); err == nil {
				t.Error("Validate accepted timings that would flap or never renew")
			}
		})
	}
}

// TestConfigValidateRejectsBadElection confirms the block is wired into the
// single validated core, so a bad `cfg fallback set` is rolled back rather than
// persisted.
func TestConfigValidateRejectsBadElection(t *testing.T) {
	base := Config{
		OAuth:    OAuthConfig{AppToken: "xapp-x", BotToken: "xoxb-x"},
		Election: ElectionConfig{LeaseSeconds: 5, RenewSeconds: 30},
	}
	if err := base.Validate(); err == nil {
		t.Fatal("Config.Validate accepted a fallback block that Validate rejects on its own")
	}

	base.Election = ElectionConfig{}
	if err := base.Validate(); err != nil {
		t.Errorf("Config.Validate rejected a default fallback block: %v", err)
	}
}
