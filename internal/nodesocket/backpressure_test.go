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

func deafPeer(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := nodesocket.Upgrade(w, r, 0)
		if err != nil {
			return
		}
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

// The design relies on gorilla blocking rather than queueing, so backpressure survives the socket hop.
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

	if second := conn.WriteMessage(payload); second == nil {
		t.Fatal("a connection that timed out mid-write accepted another frame")
	}
}

func TestTheWindowBindsBeforeTheSocketDoes(t *testing.T) {
	conn := dialDeaf(t, 30*time.Second)
	link := nodelink.New(conn, nodelink.Options{WindowBytes: nodesocket.DefaultWindowBytes})
	t.Cleanup(func() { _ = link.Close() })

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

// Guards DefaultWindowBytes: with nodelink's 4 MiB default the socket fills first and the block lands
// in Link.write, which takes no context. Tidying back to the default must fail here.
func TestTheDefaultWindowLetsTheBlockLandWhereNoContextReaches(t *testing.T) {
	writeTimeout := 3 * time.Second
	conn := dialDeaf(t, writeTimeout)
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

func jsonPadding(n int) []byte {
	return []byte(`{"pad":"` + strings.Repeat("a", n-11) + `"}`)
}
