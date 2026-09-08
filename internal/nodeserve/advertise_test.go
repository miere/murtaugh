package nodeserve

import (
	"context"
	"testing"

	"github.com/miere/murtaugh/internal/agentwire"
)

// An advertiser with no gateway attached DROPS the push and KEEPS the value.
// Both halves matter: the next connection carries the whole claim on its
// handshake answer, so queueing would replay a stale one on top of a current
// one, and forgetting it would leave a node that reconnects claiming nothing it
// had been configured with.
func TestAnUnboundAdvertiserDropsThePushAndKeepsTheClaim(t *testing.T) {
	a := NewAdvertiser(nil)
	if !a.Current().Empty() {
		t.Fatal("a fresh advertiser claims something")
	}

	ad := agentwire.Advertisement{
		Profiles: []string{"reviewer"},
		Claims:   []agentwire.AssignmentClaim{{Match: "review-*", Profile: "reviewer"}},
	}
	// No server bound: this must not block, and must not panic.
	a.Publish(context.Background(), ad)

	got := a.Current()
	if len(got.Profiles) != 1 || got.Profiles[0] != "reviewer" || len(got.Claims) != 1 {
		t.Fatalf("the advertiser held %+v after an unbound publish", got)
	}
}

// Current hands out a copy. The handshake reads it while the watcher may be
// building the next one, and a shared slice header between those two is the
// aliasing that looks safe and is not.
func TestCurrentDoesNotShareItsSlices(t *testing.T) {
	a := NewAdvertiser(nil)
	a.Publish(context.Background(), agentwire.Advertisement{
		Profiles: []string{"reviewer"},
		Claims:   []agentwire.AssignmentClaim{{Match: "review-*"}},
	})
	first := a.Current()
	first.Profiles[0] = "mutated"
	first.Claims[0].Match = "mutated"
	if second := a.Current(); second.Profiles[0] == "mutated" || second.Claims[0].Match == "mutated" {
		t.Fatal("Current handed out the advertiser's own backing arrays")
	}
}
