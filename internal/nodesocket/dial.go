package nodesocket

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type DialOptions struct {
	Token        string
	WriteTimeout time.Duration
	// An office gateway rarely has a publicly trusted certificate; a named field rather than an env var
	// means a deployment that turns verification off says so.
	InsecureSkipVerify bool
}

// Plain ws:// is refused unless the host is loopback, because the credential travels in a header;
// loopback is the --role both setup, where there is no network to intercept.
func Dial(ctx context.Context, rawURL string, opts DialOptions) (*Conn, error) {
	if strings.TrimSpace(opts.Token) == "" {
		return nil, fmt.Errorf("nodesocket: no node token to present")
	}
	endpoint, err := ResolveEndpoint(rawURL)
	if err != nil {
		return nil, err
	}

	dialer := &websocket.Dialer{
		HandshakeTimeout: handshakeTimeout,
		ReadBufferSize:   bufferSize,
		WriteBufferSize:  bufferSize,
	}
	if opts.InsecureSkipVerify {
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // deliberate, documented above
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+opts.Token)

	ws, resp, err := dialer.DialContext(ctx, endpoint, header)
	if err != nil {
		return nil, classify(endpoint, resp, err)
	}
	return newConn(ws, opts.WriteTimeout), nil
}

// Exported because a learned address needs the same wss rule: one that came from a gateway is less
// trustworthy than one an operator typed, not more.
func ResolveEndpoint(rawURL string) (string, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", fmt.Errorf("nodesocket: no gateway address")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("nodesocket: gateway address %q: %w", rawURL, err)
	}
	switch u.Scheme {
	case "wss":
	case "ws":
		if !isLoopback(u.Hostname()) {
			return "", fmt.Errorf("nodesocket: gateway address %q is plain ws:// to a non-loopback host; node credentials travel in a header, so wss:// is required", rawURL)
		}
	default:
		return "", fmt.Errorf("nodesocket: gateway address %q must be ws:// or wss://", rawURL)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = Path
	}
	return u.String(), nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
