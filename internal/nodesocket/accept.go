package nodesocket

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// BearerToken pulls the presented credential out of a request.
//
// Header only: never a query parameter. A token in a URL is written to every
// access log and proxy trace between the node and the gateway, and this one is
// the whole of a node's identity.
func BearerToken(r *http.Request) (string, bool) {
	header := strings.TrimSpace(r.Header.Get("Authorization"))
	if header == "" {
		return "", false
	}
	rest, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return "", false
	}
	token := strings.TrimSpace(rest)
	return token, token != ""
}

// Upgrade turns an authenticated HTTP request into a link transport.
//
// It is called only AFTER the credential has been verified. An upgrade
// completed before verification would give an unauthenticated peer a socket to
// hold, and refusing afterwards means the rejection arrives as a WebSocket
// close frame instead of an HTTP status a dialler can read.
func Upgrade(w http.ResponseWriter, r *http.Request, writeTimeout time.Duration) (*Conn, error) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  bufferSize,
		WriteBufferSize: bufferSize,
		// Same-origin checking is meaningless here and actively wrong: the peer
		// is a daemon, not a browser, and it sends no Origin. The credential is
		// the check, and it has already passed by the time this runs.
		CheckOrigin: func(*http.Request) bool { return true },
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written its own error response.
		return nil, fmt.Errorf("nodesocket: upgrade: %w", err)
	}
	return newConn(ws, writeTimeout), nil
}
