package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path"
	"sync"
)

// outboundGate refuses Slack Web API requests this node is no longer entitled
// to make.
//
// # Why the gate lives in the HTTP transport
//
// The obvious place to gate leadership is the socket: stop reading events and
// the node stops working. That is wrong, and dangerously so. Slack's Web API is
// stateless HTTP authenticated by the bot token, entirely independent of the
// socket. A demoted node with a closed socket can still post — and it will,
// because the chat turns it started deliberately run on background contexts so
// that a reconnect does not kill them. Closing the socket stops the node
// hearing; it does nothing to stop it speaking.
//
// So the gate belongs on the outbound side, and it belongs at the transport
// rather than at the several typed interfaces above it. This package posts to
// Slack through at least five collaborators — the socket api, the alert
// poster and editor, the channel cache's client, the streaming writers, the
// attachment uploader — and each of those is reached from paths that do not
// know about each other. Gating them individually means gating them all,
// correctly, forever, including the next one added. Gating the transport is one
// place and admits no exceptions: no HTTP request reaches Slack from this
// process unless leadership allows it.
//
// # What is exempt
//
// Two methods must work regardless, because they are how a node establishes
// leadership rather than exercises it:
//
//   - auth.test resolves the Slack identity the lock is keyed on, and backs the
//     connection watchdog's heartbeat. It is a pure read that changes nothing.
//   - apps.connections.open opens the socket, which only a leader reaches
//     anyway since the supervisor runs under the leadership-scoped context.
type outboundGate struct {
	base   http.RoundTripper
	logger *slog.Logger

	mu sync.RWMutex
	// allow reports whether leader actions are currently permitted. It is set
	// after construction because the clients this gate wraps are built before
	// the Gateway that answers the question exists. A nil allow means no
	// election is configured, in which case every request passes — the
	// single-node deployment must not pay for a feature it did not enable.
	allow func(context.Context) bool
}

// errNotLeader is returned in place of a Slack call this node may not make.
// Callers see it as an ordinary transport failure, which every Slack path here
// already handles: a failed post is logged and degraded, never fatal.
var errNotLeader = errors.New("refusing the Slack call: this node is not the leader")

// exemptMethods are the Slack API methods that bypass the gate. Kept as an
// allowlist of exemptions rather than a denylist of writes: a denylist silently
// admits every method nobody remembered to add, and the cost of that mistake is
// a second gateway answering every message.
var exemptMethods = map[string]bool{
	"auth.test":             true,
	"apps.connections.open": true,
}

// newOutboundGate wraps base, defaulting to http.DefaultTransport.
func newOutboundGate(base http.RoundTripper, logger *slog.Logger) *outboundGate {
	if base == nil {
		base = http.DefaultTransport
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &outboundGate{base: base, logger: logger}
}

// setAllow installs the leadership predicate. Called once, after the Gateway
// exists.
func (g *outboundGate) setAllow(allow func(context.Context) bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.allow = allow
}

// httpClient returns an *http.Client routing through the gate, suitable for
// slack.OptionHTTPClient.
func (g *outboundGate) httpClient() *http.Client { return &http.Client{Transport: g} }

// RoundTrip refuses the request unless the method is exempt or leadership
// currently permits it.
func (g *outboundGate) RoundTrip(req *http.Request) (*http.Response, error) {
	method := slackMethod(req)
	if exemptMethods[method] {
		return g.base.RoundTrip(req)
	}

	g.mu.RLock()
	allow := g.allow
	g.mu.RUnlock()
	if allow == nil || allow(req.Context()) {
		return g.base.RoundTrip(req)
	}

	// Debug rather than warn: after a demotion this fires once per in-flight
	// writer as each discovers it has nothing to say, and that is the gate
	// working, not a fault worth a wall of warnings.
	g.logger.Debug("blocked a Slack call from a non-leader node", "method", method)
	return nil, errNotLeader
}

// slackMethod extracts the API method name from a Slack request URL
// ("https://slack.com/api/chat.postMessage" → "chat.postMessage").
func slackMethod(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}
	return path.Base(req.URL.Path)
}
