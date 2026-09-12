package nodesocket_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/nodesocket"
)

// Without a close frame the gateway reads 1006 and logs a node that shut down on purpose as a
// failure.
func TestClosingTellsThePeerTheCloseWasOrderly(t *testing.T) {
	read := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := nodesocket.Upgrade(w, r, 0)
		if err != nil {
			read <- err
			return
		}
		defer conn.Close()
		_, err = conn.ReadMessage()
		read <- err
	}))
	t.Cleanup(server.Close)

	conn, err := nodesocket.Dial(context.Background(), "ws://"+strings.TrimPrefix(server.URL, "http://"),
		nodesocket.DialOptions{Token: "mrtg_node_0123456789abcdef_secret"})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	select {
	case err := <-read:
		if !errors.Is(err, io.EOF) {
			t.Errorf("the peer read %v, want io.EOF for an orderly close", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the peer never noticed the close")
	}
}
