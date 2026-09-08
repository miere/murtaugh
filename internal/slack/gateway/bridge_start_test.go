package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/journal"
)

// collectingRecorder keeps every journalled event so a test can assert on what
// an operator would later be able to query.
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

// startBridge is the promotion hook, and promotion happens more than once: a
// standby that takes over, stands down and takes over again runs it each time.
// Every call has to hand the runtime a LIVE context — an implementation that
// served only the first promotion, or that reused the first serve context, would
// leave every acp/claude_code agent tool-less from the second promotion onward.
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

	// Demote: the first serve context ends.
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

// A gateway with no agent runtime has nothing to serve. Promotion must not
// dereference the absent hook — that is the ordinary state of a deployment with
// no agents configured, and it is the state a gateway binary that cannot run
// agents at all is in permanently.
func TestStartBridgeWithoutARuntimeIsANoop(t *testing.T) {
	gw := &Gateway{logger: quietLogger()}
	gw.startBridge(context.Background())
}

// A failed bind costs every acp/claude_code agent every Murtaugh tool, silently:
// the agent still answers, and only fails hours later when it tries to post. A
// log line at promotion time is not enough to diagnose that, so the failure is
// journalled too, where `murtaugh journal query --stream gateway` will find it.
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
