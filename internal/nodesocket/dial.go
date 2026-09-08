package nodesocket

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// DialOptions configures the node side of the connection.
type DialOptions struct {
	// Token is the node's bearer credential. Required: an unauthenticated
	// connection is not a state this transport can reach.
	Token string
	// WriteTimeout bounds one write. Zero takes DefaultWriteTimeout.
	WriteTimeout time.Duration
	// InsecureSkipVerify turns off certificate verification. It exists because
	// an office gateway will not hold a publicly trusted certificate and the
	// alternative is an operator who cannot connect at all; it is a named,
	// greppable field rather than an ambient environment variable so a
	// deployment that uses it says so.
	InsecureSkipVerify bool
}

// Dial opens a link to the gateway at rawURL.
//
// rawURL is the gateway's seed address: ws:// or wss://, with or without the
// endpoint path — the path is supplied here so an operator configures a host
// and not a URL shape.
//
// Plain ws:// is refused unless the host is loopback. #170 makes wss mandatory
// and the credential travelling in a header is exactly why; loopback is carved
// out because that is the `--role both` deployment the split is exercised with,
// where there is no network to intercept.
func Dial(ctx context.Context, rawURL string, opts DialOptions) (*Conn, error) {
	if strings.TrimSpace(opts.Token) == "" {
		return nil, fmt.Errorf("nodesocket: no node token to present")
	}
	endpoint, err := resolveEndpoint(rawURL)
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
		return nil, dialError(endpoint, resp, err)
	}
	return newConn(ws, opts.WriteTimeout), nil
}

// resolveEndpoint normalises a seed address into the endpoint to dial.
func resolveEndpoint(rawURL string) (string, error) {
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

// dialError turns a failed upgrade into something an operator can act on. The
// bare error is "bad handshake", which is the same string for a wrong address,
// a rejected token and a standby gateway.
func dialError(endpoint string, resp *http.Response, err error) error {
	if resp == nil {
		return fmt.Errorf("nodesocket: dial %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		detail = resp.Status
	}
	return fmt.Errorf("nodesocket: dial %s: %w (%s: %s)", endpoint, err, resp.Status, detail)
}
