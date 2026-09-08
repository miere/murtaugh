package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/miere/murtaugh/internal/config"
)

// firestoreNodeTokens holds issued node credentials in Firestore, one document
// per credential keyed by its selector.
type firestoreNodeTokens struct {
	client *firestore.Client
	root   string
}

// Firestore node-token document fields. The timestamps are stored as the same
// fixed-layout strings the SQL backends use, so all three parse identically and
// a document read by hand shows the same value the operator saw in the CLI.
const (
	fsTokenSelector   = "selector"
	fsTokenSecretHash = "secret_hash"
	fsTokenNodeID     = "node_id"
	fsTokenUserID     = "user_id"
	fsTokenLabel      = "label"
	fsTokenCreatedAt  = "created_at"
	fsTokenExpiresAt  = "expires_at"
	fsTokenRevokedAt  = "revoked_at"
)

// openFirestoreNodeTokens connects to the Firestore credential store. Firestore
// has no schema to migrate: the collection appears on first write.
func openFirestoreNodeTokens(ctx context.Context, fsc config.FirestoreConfig) (config.NodeTokenStore, error) {
	client, err := newFirestoreClient(ctx, fsc)
	if err != nil {
		return nil, err
	}
	return &firestoreNodeTokens{client: client, root: fsc.EffectiveCollection()}, nil
}

func (s *firestoreNodeTokens) Close() error { return s.client.Close() }

func (s *firestoreNodeTokens) tokens() *firestore.CollectionRef {
	return s.client.Collection(s.root + "_node_tokens")
}

// tokenDocID renders a selector as a document ID. A selector is already
// lowercase hex and therefore a legal ID on its own, but it goes through the
// same escaping helper as every other document here so the collection reads
// like its neighbours.
func tokenDocID(selector string) string { return itemDocID("token", selector) }

func (s *firestoreNodeTokens) Put(ctx context.Context, token config.NodeToken) error {
	if err := token.Validate(); err != nil {
		return err
	}
	// Create, not Set: an existing selector must fail rather than overwrite a
	// live node's credential. See the SQL implementation for why.
	_, err := s.tokens().Doc(tokenDocID(token.Selector)).Create(ctx, map[string]any{
		fsTokenSelector:   token.Selector,
		fsTokenSecretHash: token.SecretHash,
		fsTokenNodeID:     token.NodeID,
		fsTokenUserID:     token.UserID,
		fsTokenLabel:      token.Label,
		fsTokenCreatedAt:  stampKey(token.CreatedAt),
		fsTokenExpiresAt:  stampKey(token.ExpiresAt),
		fsTokenRevokedAt:  stampKey(token.RevokedAt),
	})
	if err != nil {
		return fmt.Errorf("store node token for %q: %w", token.NodeID, err)
	}
	return nil
}

func (s *firestoreNodeTokens) BySelector(ctx context.Context, selector string) (config.NodeToken, bool, error) {
	snap, err := s.tokens().Doc(tokenDocID(selector)).Get(ctx)
	if status.Code(err) == codes.NotFound {
		return config.NodeToken{}, false, nil
	}
	if err != nil {
		return config.NodeToken{}, false, fmt.Errorf("look up node token: %w", err)
	}
	token, err := decodeNodeTokenDoc(snap)
	if err != nil {
		return config.NodeToken{}, false, err
	}
	return token, true, nil
}

func (s *firestoreNodeTokens) List(ctx context.Context, nodeID string) ([]config.NodeToken, error) {
	// Equality only, with the ordering done in Go. An equality filter plus an
	// OrderBy on a different field would demand a composite index that an
	// operator has to create by hand before this worked at all; the automatic
	// single-field index serves this as it stands.
	query := s.tokens().Query
	if nodeID != "" {
		query = query.Where(fsTokenNodeID, "==", nodeID)
	}
	iter := query.Documents(ctx)
	defer iter.Stop()

	var out []config.NodeToken
	for {
		snap, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("list node tokens: %w", err)
		}
		token, err := decodeNodeTokenDoc(snap)
		if err != nil {
			return nil, err
		}
		out = append(out, token)
	}
	sortNodeTokens(out)
	return out, nil
}

func (s *firestoreNodeTokens) Revoke(ctx context.Context, selector string, at time.Time) (config.NodeToken, bool, error) {
	token, found, err := s.BySelector(ctx, selector)
	if err != nil || !found {
		return config.NodeToken{}, found, err
	}
	if !token.RevokedAt.IsZero() {
		// Already revoked: the first timestamp is the audit fact and stands.
		return token, true, nil
	}
	revoked := at.UTC()
	if _, err := s.tokens().Doc(tokenDocID(selector)).Update(ctx, []firestore.Update{
		{Path: fsTokenRevokedAt, Value: stampKey(revoked)},
	}); err != nil {
		return config.NodeToken{}, false, fmt.Errorf("revoke node token: %w", err)
	}
	token.RevokedAt = revoked
	return token, true, nil
}

// decodeNodeTokenDoc reads a credential document.
//
// Unlike the job-run scan, a malformed document is an ERROR rather than a
// skipped row: a credential that cannot be read is a credential whose expiry
// and revocation cannot be read either, and treating it as absent would be
// indistinguishable from treating it as valid on the next code path that
// touches it.
func decodeNodeTokenDoc(snap *firestore.DocumentSnapshot) (config.NodeToken, error) {
	var token config.NodeToken
	for _, field := range []struct {
		name string
		into *string
	}{
		{fsTokenSelector, &token.Selector},
		{fsTokenSecretHash, &token.SecretHash},
		{fsTokenNodeID, &token.NodeID},
		{fsTokenUserID, &token.UserID},
		{fsTokenLabel, &token.Label},
	} {
		value, err := docString(snap, field.name)
		if err != nil {
			return config.NodeToken{}, fmt.Errorf("read node token: %w", err)
		}
		*field.into = value
	}
	for _, field := range []struct {
		name string
		into *time.Time
	}{
		{fsTokenCreatedAt, &token.CreatedAt},
		{fsTokenExpiresAt, &token.ExpiresAt},
		{fsTokenRevokedAt, &token.RevokedAt},
	} {
		raw, err := docString(snap, field.name)
		if err != nil {
			return config.NodeToken{}, fmt.Errorf("read node token: %w", err)
		}
		at, err := parseStamp(raw)
		if err != nil {
			return config.NodeToken{}, fmt.Errorf("read node token: %w", err)
		}
		*field.into = at
	}
	return token, nil
}
