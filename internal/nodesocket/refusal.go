package nodesocket

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// A refused handshake has to say WHICH refusal it was.
//
// #170's failover requirement is not "the node reconnects" — it reconnects
// today. It is that a node losing its gateway can tell "wrong gateway" from
// "gateway down" from "credential rejected". Those three want three different
// responses: hop to the address you were given, keep backing off against every
// address you know, and stop hoping while somebody fixes the credential. Before
// this they were one string containing the word "handshake", which is a support
// ticket with no evidence in it.
//
// The refusal travels in the HTTP handshake rather than in a close frame,
// because a close frame cannot carry it: the transport collapses every ordinary
// close code to io.EOF and discards the reason text. It also could not carry it
// in time — the point of refusing before the upgrade is that nothing is
// established to close.

const (
	// HeaderLeader is where a gateway that is not the elected leader names the
	// one that is: a space-separated address list, in the form
	// config.LeaderAddress encodes.
	//
	// A HEADER rather than the body, because the body is not reliably readable:
	// the WebSocket client truncates a rejection body at 1 KiB and this package
	// reads less than that again. Headers survive both.
	HeaderLeader = "Murtaugh-Leader"

	// StatusWrongGateway is what a gateway answers a node that reached the
	// wrong one: 421 Misdirected Request, whose meaning in RFC 9110 is exactly
	// this — the request went to a server unable to produce a response for it,
	// and should be retried elsewhere.
	//
	// Deliberately NOT a 3xx. A redirect invites an intermediary to replay the
	// request — Authorization header and all — against a host the node never
	// chose. The node makes this hop knowingly, presenting its credential to an
	// address it has decided to trust, or it does not make it.
	StatusWrongGateway = http.StatusMisdirectedRequest
)

// ErrCredentialRejected is a handshake the gateway understood and refused: this
// node's token is unknown, revoked, or not what the gateway holds a hash of.
// Retrying will not fix it, so a node that sees this says so loudly instead of
// hiding it inside an ordinary reconnect loop.
var ErrCredentialRejected = errors.New("nodesocket: the gateway rejected this node's credential")

// ErrGatewayUnavailable is a gateway that answered but cannot serve: no leader
// is elected, or its credential store is down. Nothing is wrong with this node;
// waiting is the correct response.
var ErrGatewayUnavailable = errors.New("nodesocket: the gateway is not serving runtime nodes")

// ErrGatewayUnreachable is no answer at all — refused, timed out, DNS gone,
// wifi down. It is the one that must not be confused with the others, because
// it is the only one where trying a different address can help.
var ErrGatewayUnreachable = errors.New("nodesocket: the gateway did not answer")

// RedirectError says the dialled gateway is not the elected leader, and names
// the leader where it can.
//
// Addresses may be EMPTY, and that is a distinct state rather than a degenerate
// one: it means this gateway knows it is not the leader and knows the leader
// accepts no nodes. Redialling the same place harder will not help, and neither
// will hopping — there is nowhere to hop to.
type RedirectError struct {
	// Endpoint is what was dialled, so a log line names both ends.
	Endpoint string
	// Addresses is where the leader says it can be reached, most durable first.
	Addresses []string
}

func (e *RedirectError) Error() string {
	if len(e.Addresses) == 0 {
		return fmt.Sprintf("nodesocket: %s is not the elected gateway, and the elected gateway accepts no runtime nodes", e.Endpoint)
	}
	return fmt.Sprintf("nodesocket: %s is not the elected gateway; the elected gateway is at %s",
		e.Endpoint, strings.Join(e.Addresses, ", "))
}

// leaderAddresses reads the leader header a refusal carries.
//
// Unexported: the node reads the leader list off RedirectError.Addresses, which
// is where classify puts it, and never off a raw header. Exporting it would be
// an API with one caller, in this file.
func leaderAddresses(header http.Header) []string {
	fields := strings.Fields(header.Get(HeaderLeader))
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// classify turns a failed upgrade into one of the four outcomes above, keeping
// the operator-readable detail the untyped error used to carry.
//
// The status is what discriminates, not the body: bodies are for humans and are
// truncated by two layers before they get here.
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
		// The status stays in the message even though the type now carries the
		// meaning: an operator reading a node's log is matching it against a
		// gateway's access log, and "401" is what appears there.
		return fmt.Errorf("%w: dial %s: %s: %s", ErrCredentialRejected, endpoint, resp.Status, detail)
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w: dial %s: %s: %s", ErrGatewayUnavailable, endpoint, resp.Status, detail)
	}
	// Anything else is a gateway that is not the one we think it is — a proxy, a
	// 404 from a server that is not Murtaugh at all — and is reported unclassified
	// rather than forced into a category that would make a node act on it.
	return fmt.Errorf("nodesocket: dial %s: %w (%s: %s)", endpoint, err, resp.Status, detail)
}
