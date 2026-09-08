package nodetoken

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

// ErrNotAuthorized is what every rejection of a presented token is. The
// specific reasons below wrap it, so a caller on the handshake path can answer
// with one `errors.Is` while an operator reading the log still learns why.
//
// No reason ever quotes the presented token, the selector it claimed, or the
// stored digest. An error string is the one value on this path that reliably
// reaches a log file, a Slack thread and a troubleshoot bundle, and a rejected
// credential is still a credential — the near-miss of a rotation, or the real
// token typed into the wrong gateway.
var (
	ErrNotAuthorized = errors.New("node token: not authorized")
	// ErrUnknownCredential means no credential with that selector exists.
	ErrUnknownCredential = fmt.Errorf("%w: unknown credential", ErrNotAuthorized)
	// ErrSecretMismatch means the selector exists but the secret did not match.
	ErrSecretMismatch = fmt.Errorf("%w: secret mismatch", ErrNotAuthorized)
	// ErrRevoked means the credential was revoked.
	ErrRevoked = fmt.Errorf("%w: credential revoked", ErrNotAuthorized)
	// ErrExpired means the credential is past its expiry.
	ErrExpired = fmt.Errorf("%w: credential expired", ErrNotAuthorized)
)

// Verify resolves a presented token to the node and user it was minted for.
//
// This is the function that makes "the token IS the identity" true: its only
// input from the peer is the token itself. There is no node id parameter to
// cross-check, and there is nothing a caller could pass that would let a peer
// influence which record is returned beyond presenting a credential that
// actually exists.
//
// A store error is returned as itself, NOT wrapped in ErrNotAuthorized: a
// database that is down has not told us the credential is bad, and a caller
// that conflated the two would lock a fleet out on a transient outage and log
// it as an attack.
func Verify(ctx context.Context, store config.NodeTokenStore, presented string, now time.Time) (config.NodeToken, error) {
	credential, err := Parse(presented)
	if err != nil {
		// Malformed input never reaches the store: an unauthenticated peer must
		// not be able to spend a database round trip per packet.
		return config.NodeToken{}, fmt.Errorf("%w: %w", ErrNotAuthorized, err)
	}

	record, found, err := store.BySelector(ctx, credential.Selector)
	if err != nil {
		return config.NodeToken{}, err
	}
	if !found {
		return config.NodeToken{}, ErrUnknownCredential
	}

	if !Equal(HashSecret(credential.Secret), Digest(record.SecretHash)) {
		return config.NodeToken{}, ErrSecretMismatch
	}

	// Proving the secret comes first, and the lifecycle checks come after, so
	// the distinction between "revoked" and "never existed" is only ever
	// disclosed to someone who already holds the secret.
	if !record.RevokedAt.IsZero() {
		return config.NodeToken{}, ErrRevoked
	}
	if !record.ExpiresAt.IsZero() && !now.Before(record.ExpiresAt) {
		return config.NodeToken{}, ErrExpired
	}
	return record, nil
}

// ConnectionCloser terminates the live connections authenticated with a given
// credential. It is the seam #190 asks for: revocation has to close live
// connections, not merely refuse future handshakes, and there are no
// connections yet to close.
//
// It is keyed by SELECTOR rather than by node, and that is the load-bearing
// detail. Rotation means a node holds two live credentials at once and is asked
// to drop the old one; closing "every connection of node X" would take the node
// down in the middle of the very operation the overlap exists to make seamless.
// Only the connections that authenticated with the revoked credential may be
// closed.
//
// The stage that owns live sessions (#193/#197) implements this. Until then
// Revoker.Connections is nil and revocation is a store update only — which is
// the honest state of affairs and is what RevocationLimitation says out loud.
type ConnectionCloser interface {
	CloseCredential(ctx context.Context, selector string) error
}

// RevocationLimitation states what revocation does and does not do today, for
// the CLI to print. A revoked credential stops verifying immediately; a node
// already connected with it stays connected until something implements
// ConnectionCloser.
const RevocationLimitation = "revoked credentials stop verifying immediately, but any connection already " +
	"authenticated with one stays open until the gateway learns to close it (#193)"

// Revoker revokes a credential and, when something owns live connections, tears
// down the ones that credential authenticated.
type Revoker struct {
	// Store is the credential store. Required.
	Store config.NodeTokenStore
	// Connections closes live sessions. nil means nothing owns any yet.
	Connections ConnectionCloser
}

// Revoke marks the credential revoked and closes what it authenticated.
//
// The store update happens FIRST and its result gates everything after it. A
// connection torn down before the credential was durably revoked would simply
// reconnect and re-authenticate with the same token.
//
// A selector that does not exist closes nothing: an unknown selector is a typo
// or a probe, and letting either reach the connection layer would make this a
// remote way to disturb live sessions.
func (r Revoker) Revoke(ctx context.Context, selector string, at time.Time) (config.NodeToken, bool, error) {
	if r.Store == nil {
		return config.NodeToken{}, false, errors.New("node token: revoker has no store")
	}
	record, found, err := r.Store.Revoke(ctx, selector, at)
	if err != nil || !found {
		return config.NodeToken{}, found, err
	}
	if r.Connections != nil {
		if err := r.Connections.CloseCredential(ctx, selector); err != nil {
			// The credential is already revoked and will not verify again, so
			// this is a failure to hurry rather than a failure to secure. It is
			// reported so an operator can act, with the record returned so they
			// can see what was revoked.
			return record, true, fmt.Errorf("node token: revoked, but could not close its live connections: %w", err)
		}
	}
	return record, true, nil
}
