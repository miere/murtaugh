package nodesocket

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Header only, never a query parameter: a token in a URL lands in every access log and proxy trace,
// and it is the whole of a node's identity.
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

// Call only after the credential is verified, or an unauthenticated peer holds a socket and the
// refusal arrives as a close frame instead of an HTTP status the dialler can read.
func Upgrade(w http.ResponseWriter, r *http.Request, writeTimeout time.Duration) (*Conn, error) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  bufferSize,
		WriteBufferSize: bufferSize,
		CheckOrigin:     func(*http.Request) bool { return true },
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return nil, fmt.Errorf("nodesocket: upgrade: %w", err)
	}
	return newConn(ws, writeTimeout), nil
}
