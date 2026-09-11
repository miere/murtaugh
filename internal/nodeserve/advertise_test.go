package nodeserve

import (
	"context"
	"testing"

	"github.com/miere/murtaugh/internal/agentwire"
)

func TestAnUnboundAdvertiserDropsThePushAndKeepsTheClaim(t *testing.T) {
	a := NewAdvertiser(nil)
	if !a.Current().Empty() {
		t.Fatal("a fresh advertiser claims something")
	}

	ad := agentwire.Advertisement{
		Profiles: []string{"reviewer"},
		Claims:   []agentwire.AssignmentClaim{{Match: "review-*", Profile: "reviewer"}},
	}
	a.Publish(context.Background(), ad)

	got := a.Current()
	if len(got.Profiles) != 1 || got.Profiles[0] != "reviewer" || len(got.Claims) != 1 {
		t.Fatalf("the advertiser held %+v after an unbound publish", got)
	}
}

// The handshake reads Current while the watcher may be building the next claim, so a shared slice
// would race.
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
