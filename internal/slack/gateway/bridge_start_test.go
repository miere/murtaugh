package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/journal"
)

type collectingRecorder struct {
	mu     sync.Mutex
	events []journal.Event
}

func (r *collectingRecorder) Record(_ context.Context, e journal.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *collectingRecorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, e := range r.events {
		out = append(out, e.Kind)
	}
	return out
}

// A gateway can be promoted many times, and reusing the first serve context would
// leave agents with no tools after the second promotion.
func TestStartBridgeServesToolsOnEveryPromotion(t *testing.T) {
	var mu sync.Mutex
	var got []context.Context
	served := make(chan struct{}, 4)

	gw := &Gateway{
		logger: quietLogger(),
		serveTools: func(ctx context.Context) error {
			mu.Lock()
			got = append(got, ctx)
			mu.Unlock()
			served <- struct{}{}
			<-ctx.Done()
			return nil
		},
	}

	ctx1, stop1 := context.WithCancel(context.Background())
	gw.startBridge(ctx1)
	awaitServe(t, served)

	stop1()

	ctx2, stop2 := context.WithCancel(context.Background())
	defer stop2()
	gw.startBridge(ctx2)
	awaitServe(t, served)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("serveTools ran %d times across two promotions, want 2", len(got))
	}
	if got[1].Err() != nil {
		t.Fatalf("the second promotion was handed an already-cancelled context: %v", got[1].Err())
	}
	if got[0] == got[1] {
		t.Fatal("both promotions were handed the same context; the demotion would kill the new run")
	}
}

// Having no runtime is the normal state of a gateway with no agents configured.
func TestStartBridgeWithoutARuntimeIsANoop(t *testing.T) {
	gw := &Gateway{logger: quietLogger()}
	gw.startBridge(context.Background())
}

// A failed bind silently leaves agents without Murtaugh tools until they fail
// hours later, so a log line at promotion is not enough.
func TestStartBridgeJournalsAFailureToServe(t *testing.T) {
	rec := &collectingRecorder{}
	done := make(chan struct{})
	gw := &Gateway{
		logger:   quietLogger(),
		recorder: rec,
		serveTools: func(context.Context) error {
			defer close(done)
			return errors.New("listen on /tmp/x.sock: address already in use")
		},
	}

	gw.startBridge(context.Background())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveTools was never called")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, kind := range rec.kinds() {
			if kind == "bridge.start" {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no bridge.start event was journalled; recorded kinds = %v", rec.kinds())
}

func awaitServe(t *testing.T, served <-chan struct{}) {
	t.Helper()
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("serveTools was never called")
	}
}
