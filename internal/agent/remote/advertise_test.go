package remote

import (
	"testing"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/nodelink"
)

func newAdvertiseClient(t *testing.T) (*Client, *[]agentwire.Advertisement) {
	t.Helper()
	var seen []agentwire.Advertisement
	gatewaySide, nodeSide := nodelink.Pipe(32)
	client := New(gatewaySide, Options{
		Logger:    discardLogger(),
		Advertise: AdvertiserFunc(func(ad agentwire.Advertisement) { seen = append(seen, ad) }),
	})
	t.Cleanup(func() { _ = nodeSide.Close(); _ = client.Close() })
	return client, &seen
}

func latest(t *testing.T, seen *[]agentwire.Advertisement) agentwire.Advertisement {
	t.Helper()
	if len(*seen) == 0 {
		t.Fatal("the advertiser was never told anything")
	}
	return (*seen)[len(*seen)-1]
}

func TestAnOpeningClaimYieldsToOneThatAlreadyLanded(t *testing.T) {
	client, seen := newAdvertiseClient(t)

	pushed := agentwire.Advertisement{Profiles: []string{"reviewer"}}
	opening := agentwire.Advertisement{Profiles: []string{"stale"}}

	client.applyAdvertisement(pushed, false)
	client.applyAdvertisement(opening, true)

	if got := latest(t, seen); len(got.Profiles) != 1 || got.Profiles[0] != "reviewer" {
		t.Fatalf("the registry holds %+v; the handshake's older snapshot overwrote a newer push", got)
	}
	if len(*seen) != 1 {
		t.Fatalf("the advertiser was told %d times, want 1: a suppressed claim must not reach the registry either", len(*seen))
	}
}

func TestALaterClaimReplacesTheOpeningOneWholesale(t *testing.T) {
	client, seen := newAdvertiseClient(t)

	client.applyAdvertisement(agentwire.Advertisement{
		Profiles: []string{"reviewer"},
		Claims:   []agentwire.AssignmentClaim{{Match: "review-*"}, {Match: "nc-*"}},
	}, true)
	client.applyAdvertisement(agentwire.Advertisement{Profiles: []string{"reviewer"}}, false)

	got := latest(t, seen)
	if len(got.Claims) != 0 {
		t.Fatalf("the registry holds %+v; a new claim set replaces the old one, it does not merge with it", got.Claims)
	}
	if len(*seen) != 2 {
		t.Fatalf("the advertiser was told %d times, want 2", len(*seen))
	}
}

// Shared slices would let a node that re-advertises mutate the copy delegation reads on
// another goroutine.
func TestTheAdvertiserIsHandedItsOwnCopy(t *testing.T) {
	client, seen := newAdvertiseClient(t)
	mine := agentwire.Advertisement{Profiles: []string{"reviewer"}}
	client.applyAdvertisement(mine, true)

	(*seen)[0].Profiles[0] = "mutated"
	if mine.Profiles[0] == "mutated" {
		t.Fatal("the advertiser was handed the caller's own backing array")
	}
}

func TestAClientWithNoAdvertiserDropsTheClaimRatherThanFaulting(t *testing.T) {
	gatewaySide, nodeSide := nodelink.Pipe(32)
	client := New(gatewaySide, Options{Logger: discardLogger()})
	t.Cleanup(func() { _ = nodeSide.Close(); _ = client.Close() })

	client.applyAdvertisement(agentwire.Advertisement{Profiles: []string{"reviewer"}}, true)
	client.applyAdvertisement(agentwire.Advertisement{Profiles: []string{"ops"}}, false)
	if !client.advertisedOnce {
		t.Fatal("the ordering latch did not take, so an opening claim arriving late could still roll a pushed one back")
	}
}
