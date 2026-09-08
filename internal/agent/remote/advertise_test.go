package remote

import (
	"testing"

	"github.com/miere/murtaugh/internal/agentwire"
	"github.com/miere/murtaugh/internal/nodelink"
)

// The ordering rule, which is the whole of what this package decides about
// advertisements. Two paths deliver a claim — the handshake answer, on whoever
// called Initialize, and a pushed change, on the link's request dispatch — and
// they run on different goroutines, so wire order does not survive into
// delivery order. The newest claim must win either way.

// The claim is observed where production observes it — at the Advertiser. The
// client keeps no copy of its own to read back, on purpose: the registry holds
// the claim, and a second copy here would be a second answer to "what does this
// node serve" that nothing reconciles.
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

// latest is the last claim the advertiser was handed — the registry's view.
func latest(t *testing.T, seen *[]agentwire.Advertisement) agentwire.Advertisement {
	t.Helper()
	if len(*seen) == 0 {
		t.Fatal("the advertiser was never told anything")
	}
	return (*seen)[len(*seen)-1]
}

// A pushed change that overtakes the handshake answer is not undone by it. This
// is the failure the guard exists for: the older snapshot arrives second and,
// applied, would silently roll the node's claim back to what it was at connect.
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

// In the ordinary order the opening claim lands and a later push replaces it
// wholesale — never merges with it, because there is no ordering guarantee a
// merge could be made correct against.
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

// The claim handed to the registry must not share slices with the caller's, or
// a node re-advertising mutates the copy the registry is already holding — and
// delegation reads that copy from another goroutine.
func TestTheAdvertiserIsHandedItsOwnCopy(t *testing.T) {
	client, seen := newAdvertiseClient(t)
	mine := agentwire.Advertisement{Profiles: []string{"reviewer"}}
	client.applyAdvertisement(mine, true)

	(*seen)[0].Profiles[0] = "mutated"
	if mine.Profiles[0] == "mutated" {
		t.Fatal("the advertiser was handed the caller's own backing array")
	}
}

// A gateway with no registry bound drops the claim rather than faulting on it.
// The node cannot know whether the gateway keeps one, and failing its push
// would make the node's own logs blame it for the gateway's shape.
//
// There is nothing else left to assert here, and that is the point of the
// shape: with no advertiser there is no second copy of the claim to have
// recorded it in.
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
