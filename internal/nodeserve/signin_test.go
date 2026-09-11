package nodeserve_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/agent/remote"
	"github.com/miere/murtaugh/internal/nodelink"
	"github.com/miere/murtaugh/internal/nodeserve"
)

func TestASignInTheNodeStopsWaitingOnIsWithdrawnAtTheGateway(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	updates := make(chan agent.SignInState, 2)
	gatewaySide, nodeSide := nodelink.Pipe(32)
	gateway := remote.New(gatewaySide, remote.Options{Logger: logger, Owner: "UOWNER",
		SignIns: func(ctx context.Context, _ *agent.SignInPrompt, settled <-chan agent.SignInSettled, _ func(error)) {
			for {
				select {
				case u := <-settled:
					updates <- u.State
					if u.State.Terminal() {
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}})
	t.Cleanup(func() { _ = gateway.Close() })
	signIns := nodeserve.NewSignIns(logger)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = nodeserve.Serve(ctx, nodeSide, nodeserve.UnconfiguredClient{}, nodeserve.Options{Logger: logger, SignIns: signIns})
	}()
	if err := gateway.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	waiting, stop := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stop()
	if _, ok := signIns.SignIn(waiting, agent.SignInRequest{Tool: "gcp-mcp", Profile: "gcloud"}); ok {
		t.Fatal("a sign-in the gateway never showed was reported as shown")
	}
	select {
	case state := <-updates:
		if state != agent.SignInCancelled {
			t.Fatalf("the sign-in the node gave up on was settled as %q", state)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sign-in the node gave up on was left open at the gateway")
	}
}
