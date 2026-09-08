package config

import (
	"context"
	"errors"
	"strings"
	"time"
)

// This file defines the node-credential seam: the record the gateway keeps for
// every bearer token it has issued to a runtime node, and the store that holds
// them (#190, part of the split in #170).
//
// It sits beside the leader lock and the scheduled-run claim for the same
// reason those do: it is state that nodes serving one Slack app must agree on,
// and the store their shared configuration came from is the only place they
// already agree on. And like those two it is deliberately NOT a config section
// — it is absent from AllSections and AllSingletons, so token records never
// enter the Config every process loads, never appear in `cfg show`, and are
// never carried by Snapshot/Restore.
//
// The last of those has a consequence worth stating plainly rather than
// discovering: `cfg db migrate --to <backend>` does NOT carry node tokens
// across. Nodes must be re-enrolled after a backend migration, exactly as
// job_runs and leader_locks are re-established rather than copied.
//
// One row per TOKEN, not per node. Two live tokens for one node is then just
// two rows sharing a node ID, which is what makes rotation a matter of minting
// early and revoking late rather than a schema with a "previous token" column
// that hard-codes the overlap at two and cannot express per-credential expiry,
// labels, or "revoke this one leaked credential".

// NodeToken is one issued credential.
//
// It carries a DIGEST, never a token: the plaintext exists only at the moment
// nodetoken.Mint returns it. Nothing in this type, and no method on
// NodeTokenStore, can produce one.
type NodeToken struct {
	// Selector is the credential id — the public half of the token, and the
	// store key. Not a secret: it is printed by `node token list` and named by
	// `node token revoke`.
	Selector string
	// SecretHash is the SHA-256 of the token's secret half, lowercase hex.
	// Typed as a plain string here so this package keeps its stdlib-only
	// surface; nodetoken.Digest is the type that carries the comparison rule.
	SecretHash string
	// NodeID is the node this credential identifies. The gateway resolves it
	// FROM the token; a node never asserts it.
	NodeID string
	// UserID is the Murtaugh user the node acts for, resolved the same way and
	// for the same reason.
	UserID string
	// Label is the operator's note about where this credential lives ("mac
	// mini", "rotation 2026-09"). Free text, no meaning to the code.
	Label string
	// CreatedAt is when the credential was minted, UTC.
	CreatedAt time.Time
	// ExpiresAt is when it stops verifying, UTC. The zero time means it never
	// expires on its own and only revocation ends it.
	ExpiresAt time.Time
	// RevokedAt is when it was revoked, UTC. The zero time means it is live.
	RevokedAt time.Time
}

// Live reports whether the credential verifies at `at`: not revoked, and not
// past its expiry.
func (t NodeToken) Live(at time.Time) bool {
	if !t.RevokedAt.IsZero() {
		return false
	}
	return t.ExpiresAt.IsZero() || at.Before(t.ExpiresAt)
}

// Validate reports whether the record is usable.
//
// NodeID and UserID are both required. Carrying UserID but leaving it
// unchecked would make "the gateway resolves token → node → user" a half-truth:
// a record with an empty user resolves to nobody, and the handshake that will
// eventually consume it would have to invent a fallback. Requiring it now costs
// one CLI flag and avoids that.
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

// NodeTokenStore holds the issued credentials.
//
// Note what is missing: there is no method that returns a token. The only
// values that cross this interface are digests and metadata, so "no code path
// can read a token back out of the store" is a property of the contract rather
// than a discipline callers have to keep.
type NodeTokenStore interface {
	// Put records a newly minted credential. A selector that already exists is
	// an error rather than an overwrite: a Put that silently replaced a live
	// credential would be an undetectable way to hijack a node.
	Put(ctx context.Context, token NodeToken) error

	// BySelector returns the credential with the given selector. A false with a
	// nil error means no such credential — an ordinary outcome on an
	// unauthenticated path, not a failure.
	BySelector(ctx context.Context, selector string) (NodeToken, bool, error)

	// List returns the credentials for a node, newest first. An empty nodeID
	// lists every node's credentials, which is what `node token list` with no
	// --node shows.
	List(ctx context.Context, nodeID string) ([]NodeToken, error)

	// Revoke marks the credential revoked at `at` and returns it as stored. A
	// false with a nil error means there is no such selector.
	//
	// An already-revoked credential keeps its original RevokedAt: the first
	// revocation is the audit fact, and a second call must not rewrite when the
	// credential stopped being trusted.
	Revoke(ctx context.Context, selector string, at time.Time) (NodeToken, bool, error)

	// Close releases the backend handle.
	Close() error
}
