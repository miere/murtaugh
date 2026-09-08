package nodesocket_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/nodelink"
	"github.com/miere/murtaugh/internal/nodesocket"
)

// These are #193's unproven part: what the node's WebSocket does on write when
// the gateway stops reading. It decides whether the hop costs a round trip or
// silently grows an unbounded queue with the pacing signal gone.
//
// They are written against a REAL deaf gorilla peer — a server that upgrades
// and then never calls ReadMessage — and not against nodelink.Pipe. The pipe
// cannot pose this failure: its write unblocks on either side's close, so "the
// peer is alive and simply not reading" is a state it has no way to be in, and
// a test written over it would pass for the wrong reason.

// deafPeer upgrades and then never reads. It is what a gateway whose renderer
// is stuck inside a Slack upload looks like from the node.
func deafPeer(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := nodesocket.Upgrade(w, r, 0)
		if err != nil {
			return
		}
		// Held open, never read. Closed when the test's server shuts down.
		t.Cleanup(func() { _ = conn.Close() })
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	return "ws://" + strings.TrimPrefix(server.URL, "http://")
}

func dialDeaf(t *testing.T, writeTimeout time.Duration) *nodesocket.Conn {
	t.Helper()
	conn, err := nodesocket.Dial(context.Background(), deafPeer(t), nodesocket.DialOptions{
		Token:        "mrtg_node_0123456789abcdef_secret",
		WriteTimeout: writeTimeout,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

// THE MEASUREMENT. gorilla's WriteMessage blocks; it does not queue. The bound
// is the two kernel socket buffers and nothing else — there is no goroutine and
// no backlog inside the library, so the pacing signal survives the hop instead
// of being absorbed by an invisible queue.
func TestWriteBlocksRatherThanQueueingWithoutBound(t *testing.T) {
	conn := dialDeaf(t, 3*time.Second)

	payload := make([]byte, 4<<10)
	var written atomic.Int64
	failed := make(chan error, 1)
	go func() {
		for {
			if err := conn.WriteMessage(payload); err != nil {
				failed <- err
				return
			}
			written.Add(int64(len(payload)))
		}
	}()

	var err error
	select {
	case err = <-failed:
	case <-time.After(30 * time.Second):
		t.Fatalf("the writer never blocked: it accepted %d bytes for a peer that never read one", written.Load())
	}

	bytes := written.Load()
	t.Logf("MEASURED: a deaf peer absorbed %d bytes (%d KiB) in 4 KiB frames before the write blocked; "+
		"the blocked write then failed with %v", bytes, bytes/1024, err)

	// The number itself is the kernel's business and varies by host. What must
	// hold is that it is BOUNDED and small enough that nodelink's window — which
	// is what makes the block cancellable — is the binding constraint first.
	if bytes <= 0 {
		t.Fatal("nothing was written at all")
	}
	if bytes >= 8<<20 {
		t.Fatalf("the transport absorbed %d bytes for a peer that read none: that is an unbounded queue, and the pacing signal does not survive the hop", bytes)
	}
	if bytes <= int64(nodesocket.DefaultWindowBytes) {
		t.Fatalf("the socket absorbed only %d bytes, which is at or below the %d-byte window: "+
			"the window would no longer be the first thing to bind, so the block would land in the uncancellable write",
			bytes, nodesocket.DefaultWindowBytes)
	}

	// A timed-out write is fatal to the connection, not to the frame: gorilla
	// latches the error and every later write returns it. Anything that treated
	// it as retryable would believe it was still delivering.
	if second := conn.WriteMessage(payload); second == nil {
		t.Fatal("a connection that timed out mid-write accepted another frame")
	}
}

// The window is what makes a stalled peer a cancellable wait instead of a wedge.
// nodelink.Send parks in awaitRoom, which honours the context; the transport
// write does not take one at all.
func TestTheWindowBindsBeforeTheSocketDoes(t *testing.T) {
	conn := dialDeaf(t, 30*time.Second)
	link := nodelink.New(conn, nodelink.Options{WindowBytes: nodesocket.DefaultWindowBytes})
	t.Cleanup(func() { _ = link.Close() })

	// A link payload is carried as JSON inside the envelope, so it has to BE
	// JSON. The padding is what makes the frame the size the window counts.
	payload := jsonPadding(4 << 10)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	sent := 0
	start := time.Now()
	var err error
	for err == nil {
		err = link.Send(ctx, payload)
		if err == nil {
			sent++
		}
	}
	elapsed := time.Since(start)

	frames, bytes := link.Pending()
	t.Logf("MEASURED: with a %d-byte window, %d frames (%d unacknowledged bytes, %d frames pending) "+
		"went out before Send blocked; it then honoured a 2s context and returned after %s with %v",
		nodesocket.DefaultWindowBytes, sent, bytes, frames, elapsed.Round(time.Millisecond), err)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Send returned %v, want the context's deadline — the block landed somewhere the caller cannot walk away from", err)
	}
	if bytes > nodesocket.DefaultWindowBytes+len(payload) {
		t.Fatalf("%d unacknowledged bytes are in flight against a %d-byte window", bytes, nodesocket.DefaultWindowBytes)
	}
}

// The contrast, and the reason DefaultWindowBytes exists. nodelink's own 4 MiB
// default is larger than the socket buffers, so the socket fills first and the
// block lands in Link.write — which takes no context. The caller's deadline is
// then simply ignored, and only the transport's write deadline ends the wait.
//
// This is asserted rather than described because it is the failure a later
// change would reintroduce by "tidying up" an explicit window back to the
// default.
func TestTheDefaultWindowLetsTheBlockLandWhereNoContextReaches(t *testing.T) {
	writeTimeout := 3 * time.Second
	conn := dialDeaf(t, writeTimeout)
	// No WindowBytes: nodelink's 4 MiB default, which is larger than the
	// socket buffers this transport actually has.
	link := nodelink.New(conn, nodelink.Options{})
	t.Cleanup(func() { _ = link.Close() })

	payload := jsonPadding(64 << 10)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	var err error
	for err == nil {
		err = link.Send(ctx, payload)
	}
	elapsed := time.Since(start)

	t.Logf("MEASURED: with nodelink's 4 MiB default window over the same socket, Send ignored a 500ms "+
		"context and returned after %s with %v", elapsed.Round(time.Millisecond), err)

	if elapsed < writeTimeout {
		t.Skipf("this host's socket buffers (or scheduler) let the send finish in %s, "+
			"so the uncancellable path was not reached; the window default is still the wrong one to ship", elapsed)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Send honoured its context after %s: if that is now true by construction, "+
			"nodesocket.DefaultWindowBytes has stopped earning its keep and this test should be deleted deliberately", elapsed)
	}
}

// jsonPadding is a valid JSON object of about n bytes.
func jsonPadding(n int) []byte {
	return []byte(`{"pad":"` + strings.Repeat("a", n-11) + `"}`)
}
