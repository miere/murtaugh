package config

import (
	"context"
	"errors"
	"strings"
	"time"
)

// NodeToken holds a digest, never the token: the plaintext exists only when nodetoken.Mint returns it.
type NodeToken struct {
	// Selector is not a secret: `node token list` prints it and `node token revoke` takes it.
	Selector string
	// SecretHash is a plain string so this package stays stdlib-only; nodetoken.Digest owns the comparison.
	SecretHash string
	// The gateway resolves NodeID from the token; a node never asserts it.
	NodeID string
	// UserID is resolved from the token too; sign-ins and access checks can only match a Slack user ID.
	UserID    string
	Label     string
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt time.Time
}

func (t NodeToken) Live(at time.Time) bool {
	if !t.RevokedAt.IsZero() {
		return false
	}
	return t.ExpiresAt.IsZero() || at.Before(t.ExpiresAt)
}

// UserID is required too: a record with an empty user would resolve to nobody, and the handshake
// would have to invent a fallback.
func (t NodeToken) Validate() error {
	if strings.TrimSpace(t.Selector) == "" {
		return errors.New("node token: selector is required")
	}
	if strings.TrimSpace(t.SecretHash) == "" {
		return errors.New("node token: secret hash is required")
	}
	if strings.TrimSpace(t.NodeID) == "" {
		return errors.New("node token: node id is required")
	}
	if strings.TrimSpace(t.UserID) == "" {
		return errors.New("node token: user id is required")
	}
	if t.CreatedAt.IsZero() {
		return errors.New("node token: created-at is required")
	}
	return nil
}

// No method returns a token, only digests and metadata. `cfg db migrate` does not carry tokens
// across, so nodes must be re-enrolled after a backend migration.
type NodeTokenStore interface {
	// Put refuses an existing selector rather than overwriting it: a silent replace would be an
	// undetectable way to hijack a node.
	Put(ctx context.Context, token NodeToken) error

	BySelector(ctx context.Context, selector string) (NodeToken, bool, error)

	List(ctx context.Context, nodeID string) ([]NodeToken, error)

	// Revoking twice keeps the first RevokedAt, because the first revocation is the audit fact.
	Revoke(ctx context.Context, selector string, at time.Time) (NodeToken, bool, error)

	Close() error
}
