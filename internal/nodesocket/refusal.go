package nodesocket

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// A header, not the body: the WebSocket client truncates a rejection body at 1 KiB and this package
	// reads even less. Headers survive both.
	HeaderLeader = "Murtaugh-Leader"

	// Not a 3xx: a redirect invites an intermediary to replay the request, Authorization header and
	// all, against a host the node never chose.
	StatusWrongGateway = http.StatusMisdirectedRequest
)

// Retrying will not fix it, so a node says so loudly instead of hiding it in its reconnect loop.
var ErrCredentialRejected = errors.New("nodesocket: the gateway rejected this node's credential")

// Nothing is wrong with this node, so waiting is the right response.
var ErrGatewayUnavailable = errors.New("nodesocket: the gateway is not serving runtime nodes")

// The only one of these where trying a different address can help.
var ErrGatewayUnreachable = errors.New("nodesocket: the gateway did not answer")

// Empty Addresses is a distinct state: this gateway knows the leader accepts no nodes, so neither
// redialling nor hopping elsewhere will help.
type RedirectError struct {
	Endpoint  string
	Addresses []string
}

func (e *RedirectError) Error() string {
	if len(e.Addresses) == 0 {
		return fmt.Sprintf("nodesocket: %s is not the elected gateway, and the elected gateway accepts no runtime nodes", e.Endpoint)
	}
	return fmt.Sprintf("nodesocket: %s is not the elected gateway; the elected gateway is at %s",
		e.Endpoint, strings.Join(e.Addresses, ", "))
}

func leaderAddresses(header http.Header) []string {
	fields := strings.Fields(header.Get(HeaderLeader))
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func classify(endpoint string, resp *http.Response, err error) error {
	if resp == nil {
		return fmt.Errorf("%w: dial %s: %w", ErrGatewayUnreachable, endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		detail = resp.Status
	}

	switch resp.StatusCode {
	case StatusWrongGateway:
		return &RedirectError{Endpoint: endpoint, Addresses: leaderAddresses(resp.Header)}
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: dial %s: %s: %s", ErrCredentialRejected, endpoint, resp.Status, detail)
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w: dial %s: %s: %s", ErrGatewayUnavailable, endpoint, resp.Status, detail)
	}
	return fmt.Errorf("nodesocket: dial %s: %w (%s: %s)", endpoint, err, resp.Status, detail)
}
