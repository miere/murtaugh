package gateway

import (
	"context"
	"testing"

	"github.com/miere/murtaugh/internal/credwarden"
)

func TestStartBackgroundIsNoOpWithoutAWarden(t *testing.T) {
	g := &Gateway{}
	g.StartBackground(context.Background()) // no claude_code agent: nothing to run
	g.StopBackground()                      // and stopping what never started is safe
}

// A configuration reload builds a replacement gateway. If the outgoing one kept
// its warden, two would run against the same credential and race the server's
// rotation of the refresh token — the failure the warden exists to prevent.
func TestStopBackgroundIsIdempotent(t *testing.T) {
	g := &Gateway{credWarden: credwarden.New(credwarden.Options{
		Identities: []credwarden.Identity{{Command: "/bin/claude"}},
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	g.StartBackground(ctx)
	g.StartBackground(ctx) // second call must not start a second warden
	g.StopBackground()
	g.StopBackground() // and stopping twice must not panic
}
