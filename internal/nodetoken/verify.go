package nodetoken

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

// Every reason wraps ErrNotAuthorized and never quotes the token: error strings
// reach logs, Slack threads and troubleshoot bundles.
var (
	ErrNotAuthorized     = errors.New("node token: not authorized")
	ErrUnknownCredential = fmt.Errorf("%w: unknown credential", ErrNotAuthorized)
	ErrSecretMismatch    = fmt.Errorf("%w: secret mismatch", ErrNotAuthorized)
	ErrRevoked           = fmt.Errorf("%w: credential revoked", ErrNotAuthorized)
	ErrExpired           = fmt.Errorf("%w: credential expired", ErrNotAuthorized)
)

// A store error is returned unwrapped: a database that is down has not said
// the credential is bad, and treating it so would lock the fleet out.
func Verify(ctx context.Context, store config.NodeTokenStore, presented string, now time.Time) (config.NodeToken, error) {
	credential, err := Parse(presented)
	if err != nil {
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

	if !record.RevokedAt.IsZero() {
		return config.NodeToken{}, ErrRevoked
	}
	if !record.ExpiresAt.IsZero() && !now.Before(record.ExpiresAt) {
		return config.NodeToken{}, ErrExpired
	}
	return record, nil
}

// Keyed by selector, not node: mid-rotation a node holds two credentials, and
// closing by node would drop it.
type ConnectionCloser interface {
	CloseCredential(ctx context.Context, selector string) error
}

const RevocationLimitation = "revoked credentials stop verifying immediately, but any connection already " +
	"authenticated with one stays open until the gateway learns to close it (#193)"

type Revoker struct {
	Store       config.NodeTokenStore
	Connections ConnectionCloser
}

// The store update comes first: a connection closed before the revocation is
// durable would reconnect with the same token.
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
			return record, true, fmt.Errorf("node token: revoked, but could not close its live connections: %w", err)
		}
	}
	return record, true, nil
}
