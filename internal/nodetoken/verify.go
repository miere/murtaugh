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
	if err := standing(record, now); err != nil {
		return config.NodeToken{}, err
	}
	return record, nil
}

// The secret was proven at the handshake, so a live connection is re-checked
// on what can change since; a store error is still not a rejection.
func Recheck(ctx context.Context, store config.NodeTokenStore, selector string, now time.Time) error {
	record, found, err := store.BySelector(ctx, selector)
	if err != nil {
		return err
	}
	if !found {
		return ErrUnknownCredential
	}
	return standing(record, now)
}

func standing(record config.NodeToken, now time.Time) error {
	if !record.RevokedAt.IsZero() {
		return ErrRevoked
	}
	if !record.ExpiresAt.IsZero() && !now.Before(record.ExpiresAt) {
		return ErrExpired
	}
	return nil
}

// Keyed by selector, not node: mid-rotation a node holds two credentials, and
// closing by node would drop it.
type ConnectionCloser interface {
	CloseCredential(ctx context.Context, selector string) error
}

// Polled because revocation is usually written by the CLI, a separate process
// that cannot reach the gateway's sockets.
const RecheckInterval = 10 * time.Second

var RevocationNotice = fmt.Sprintf("a revoked credential stops verifying immediately, and the gateway "+
	"drops any live connection using it within %s", RecheckInterval)

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
