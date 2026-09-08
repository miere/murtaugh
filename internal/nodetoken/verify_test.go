package nodetoken

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

// memStore is an in-memory config.NodeTokenStore. The three shipped backends are
// held to this same contract in internal/config/store; here the store is a fake
// so the verification rules are tested without a database in sight.
type memStore struct {
	records map[string]config.NodeToken
	// failWith, when set, makes every read fail — the "database is down" case.
	failWith error
	lookups  int
}

func newMemStore() *memStore { return &memStore{records: map[string]config.NodeToken{}} }

func (s *memStore) Put(_ context.Context, token config.NodeToken) error {
	if err := token.Validate(); err != nil {
		return err
	}
	if _, exists := s.records[token.Selector]; exists {
		return errors.New("selector already exists")
	}
	s.records[token.Selector] = token
	return nil
}

func (s *memStore) BySelector(_ context.Context, selector string) (config.NodeToken, bool, error) {
	s.lookups++
	if s.failWith != nil {
		return config.NodeToken{}, false, s.failWith
	}
	record, ok := s.records[selector]
	return record, ok, nil
}

func (s *memStore) List(_ context.Context, nodeID string) ([]config.NodeToken, error) {
	if s.failWith != nil {
		return nil, s.failWith
	}
	var out []config.NodeToken
	for _, r := range s.records {
		if nodeID == "" || r.NodeID == nodeID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *memStore) Revoke(_ context.Context, selector string, at time.Time) (config.NodeToken, bool, error) {
	record, ok := s.records[selector]
	if !ok {
		return config.NodeToken{}, false, nil
	}
	if record.RevokedAt.IsZero() {
		record.RevokedAt = at.UTC()
		s.records[selector] = record
	}
	return record, true, nil
}

func (s *memStore) Close() error { return nil }

var now = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

// enrol mints a credential for a node and stores it, returning the plaintext.
func enrol(t *testing.T, store *memStore, nodeID, userID string, expiresAt time.Time) string {
	t.Helper()
	minted, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	err = store.Put(context.Background(), config.NodeToken{
		Selector:   minted.Selector,
		SecretHash: string(minted.SecretHash),
		NodeID:     nodeID,
		UserID:     userID,
		CreatedAt:  now,
		ExpiresAt:  expiresAt,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return minted.Token
}

// TestVerifyResolvesTheIdentityFromTheTokenAlone is the property the whole
// design turns on (#190): a node presents a credential and nothing else, and the
// gateway is the one that says which node and which user that is.
//
// The test is written so that the only input from the "node" is the token
// string. There is no node id to pass, so an impersonating node has nothing to
// assert — which is the point being checked.
func TestVerifyResolvesTheIdentityFromTheTokenAlone(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()
	alphaToken := enrol(t, store, "alpha", "U-alpha", time.Time{})
	betaToken := enrol(t, store, "beta", "U-beta", time.Time{})

	alpha, err := Verify(ctx, store, alphaToken, now)
	if err != nil {
		t.Fatalf("Verify(alpha): %v", err)
	}
	if alpha.NodeID != "alpha" || alpha.UserID != "U-alpha" {
		t.Fatalf("alpha's token resolved to node %q user %q", alpha.NodeID, alpha.UserID)
	}

	// The second node's token must resolve to the second node. Two credentials
	// that both resolved to whoever was looked up first would pass a
	// single-credential test.
	beta, err := Verify(ctx, store, betaToken, now)
	if err != nil {
		t.Fatalf("Verify(beta): %v", err)
	}
	if beta.NodeID != "beta" || beta.UserID != "U-beta" {
		t.Fatalf("beta's token resolved to node %q user %q", beta.NodeID, beta.UserID)
	}
}

// TestVerifyRejectsATamperedToken: a token with one character changed must not
// verify, however plausible the rest of it is.
func TestVerifyRejectsATamperedToken(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()
	token := enrol(t, store, "alpha", "U-alpha", time.Time{})
	credential, err := Parse(token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// The selector is left intact so the lookup SUCCEEDS and the secret
	// comparison is what has to do the rejecting — otherwise this would only be
	// testing that an unknown selector is unknown.
	tampered := Prefix + credential.Selector + "_" + flipLastChar(credential.Secret)
	if _, err := Verify(ctx, store, tampered, now); !errors.Is(err, ErrSecretMismatch) {
		t.Fatalf("Verify(tampered secret) = %v, want ErrSecretMismatch", err)
	}
	if _, err := Verify(ctx, store, tampered, now); !errors.Is(err, ErrNotAuthorized) {
		t.Fatal("a rejection must satisfy errors.Is(err, ErrNotAuthorized) so one check covers the handshake path")
	}

	// A valid-looking token for a selector nobody issued.
	unknown := Prefix + strings.Repeat("ab", 8) + "_" + credential.Secret
	if _, err := Verify(ctx, store, unknown, now); !errors.Is(err, ErrUnknownCredential) {
		t.Fatalf("Verify(unknown selector) = %v, want ErrUnknownCredential", err)
	}
}

// TestVerifyRejectsMalformedInputWithoutTouchingTheStore: an unauthenticated
// peer must not be able to spend a database round trip per packet.
func TestVerifyRejectsMalformedInputWithoutTouchingTheStore(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()
	_ = enrol(t, store, "alpha", "U-alpha", time.Time{})

	before := store.lookups
	for _, presented := range []string{"", "hello", "xoxb-123456789012-abcdef", "mrtg_node_", "mrtg_node_zz_secret"} {
		if _, err := Verify(ctx, store, presented, now); !errors.Is(err, ErrNotAuthorized) {
			t.Errorf("Verify(%q) = %v, want a rejection", presented, err)
		}
	}
	if store.lookups != before {
		t.Fatalf("%d store lookups happened for malformed input; a malformed string must be rejected before the store is consulted",
			store.lookups-before)
	}
}

func TestVerifyRejectsRevokedAndExpiredCredentials(t *testing.T) {
	ctx := context.Background()

	t.Run("revoked", func(t *testing.T) {
		store := newMemStore()
		token := enrol(t, store, "alpha", "U-alpha", time.Time{})
		if _, err := Verify(ctx, store, token, now); err != nil {
			t.Fatalf("the credential did not verify before revocation: %v", err)
		}
		if _, _, err := store.Revoke(ctx, mustParse(t, token).Selector, now); err != nil {
			t.Fatal(err)
		}
		if _, err := Verify(ctx, store, token, now); !errors.Is(err, ErrRevoked) {
			t.Fatalf("Verify(revoked) = %v, want ErrRevoked", err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		store := newMemStore()
		expiry := now.Add(time.Hour)
		token := enrol(t, store, "alpha", "U-alpha", expiry)

		if _, err := Verify(ctx, store, token, expiry.Add(-time.Second)); err != nil {
			t.Fatalf("the credential did not verify a second before expiry: %v", err)
		}
		// The boundary is exclusive: at the expiry instant it is already over.
		if _, err := Verify(ctx, store, token, expiry); !errors.Is(err, ErrExpired) {
			t.Fatalf("Verify(at expiry) = %v, want ErrExpired", err)
		}
		if _, err := Verify(ctx, store, token, expiry.Add(time.Hour)); !errors.Is(err, ErrExpired) {
			t.Fatalf("Verify(after expiry) = %v, want ErrExpired", err)
		}
	})

	t.Run("no expiry set", func(t *testing.T) {
		store := newMemStore()
		token := enrol(t, store, "alpha", "U-alpha", time.Time{})
		if _, err := Verify(ctx, store, token, now.Add(100*365*24*time.Hour)); err != nil {
			t.Fatalf("a credential with no expiry stopped verifying: %v", err)
		}
	})
}

// TestVerifyDoesNotTreatAStoreFailureAsARejection: a database that is down has
// not told us the credential is bad. Conflating the two locks a fleet out on a
// transient outage and logs it as an attack.
func TestVerifyDoesNotTreatAStoreFailureAsARejection(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()
	token := enrol(t, store, "alpha", "U-alpha", time.Time{})

	boom := errors.New("connection refused")
	store.failWith = boom

	_, err := Verify(ctx, store, token, now)
	if !errors.Is(err, boom) {
		t.Fatalf("Verify = %v, want the store's own error", err)
	}
	if errors.Is(err, ErrNotAuthorized) {
		t.Fatal("a store outage was reported as a failed authorisation")
	}
}

// TestTwoCredentialsForOneNodeVerifyAtOnceThenOneIsRevoked is the rotation
// property, named for exactly what it checks rather than for "rotation".
//
// Two credentials are minted for the same node; BOTH must resolve to that node
// at the same instant, so the new one can be deployed before the old one is
// withdrawn. Then the first is revoked and must stop verifying while the second
// keeps working — which is the half that makes the overlap a rotation rather
// than just a second key.
func TestTwoCredentialsForOneNodeVerifyAtOnceThenOneIsRevoked(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()

	outgoing := enrol(t, store, "alpha", "U-alpha", time.Time{})
	incoming := enrol(t, store, "alpha", "U-alpha", time.Time{})
	if outgoing == incoming {
		t.Fatal("the two credentials are identical; there is no overlap to test")
	}

	for name, token := range map[string]string{"outgoing": outgoing, "incoming": incoming} {
		record, err := Verify(ctx, store, token, now)
		if err != nil {
			t.Fatalf("the %s credential did not verify during the overlap: %v", name, err)
		}
		if record.NodeID != "alpha" {
			t.Fatalf("the %s credential resolved to node %q, want alpha", name, record.NodeID)
		}
	}

	if _, _, err := store.Revoke(ctx, mustParse(t, outgoing).Selector, now); err != nil {
		t.Fatal(err)
	}

	if _, err := Verify(ctx, store, outgoing, now); !errors.Is(err, ErrRevoked) {
		t.Fatalf("the withdrawn credential still verifies: %v", err)
	}
	if _, err := Verify(ctx, store, incoming, now); err != nil {
		t.Fatalf("revoking the outgoing credential broke the incoming one — the rotation had downtime after all: %v", err)
	}
}

// closerSpy records what the revocation seam was asked to tear down.
type closerSpy struct {
	closed []string
	err    error
}

func (c *closerSpy) CloseCredential(_ context.Context, selector string) error {
	c.closed = append(c.closed, selector)
	return c.err
}

// TestRevokerClosesOnlyTheRevokedCredential covers the seam #190 asks to be left
// for connections, and the granularity decision inside it: closing "every
// connection of node X" would take the node down mid-rotation, which is the
// downtime the two-credential overlap exists to avoid.
func TestRevokerClosesOnlyTheRevokedCredential(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()
	outgoing := mustParse(t, enrol(t, store, "alpha", "U-alpha", time.Time{})).Selector
	incoming := mustParse(t, enrol(t, store, "alpha", "U-alpha", time.Time{})).Selector

	spy := &closerSpy{}
	revoker := Revoker{Store: store, Connections: spy}

	record, found, err := revoker.Revoke(ctx, outgoing, now)
	if err != nil || !found {
		t.Fatalf("Revoke: found=%v err=%v", found, err)
	}
	if record.RevokedAt.IsZero() {
		t.Fatal("Revoke returned a record that is not marked revoked")
	}
	if len(spy.closed) != 1 || spy.closed[0] != outgoing {
		t.Fatalf("closed %v, want exactly the revoked credential %q", spy.closed, outgoing)
	}
	if spy.closed[0] == incoming {
		t.Fatal("revoking the outgoing credential closed the incoming one's connections")
	}
}

// TestRevokerDoesNotReachTheConnectionLayerForAnUnknownSelector: an unknown
// selector is a typo or a probe, and letting either reach live sessions would
// make revocation a way to disturb them.
func TestRevokerDoesNotReachTheConnectionLayerForAnUnknownSelector(t *testing.T) {
	store := newMemStore()
	spy := &closerSpy{}
	revoker := Revoker{Store: store, Connections: spy}

	_, found, err := revoker.Revoke(context.Background(), "ffffffffffffffff", now)
	if err != nil {
		t.Fatalf("Revoke on an unknown selector errored: %v", err)
	}
	if found {
		t.Fatal("Revoke claimed to revoke a credential that does not exist")
	}
	if len(spy.closed) != 0 {
		t.Fatalf("an unknown selector reached the connection layer: %v", spy.closed)
	}
}

// TestRevokerReportsACloseFailureButKeepsTheRevocation: the credential is
// already durably revoked, so a failure to hurry is not a failure to secure —
// but it must still be surfaced, with the record, rather than swallowed.
func TestRevokerReportsACloseFailureButKeepsTheRevocation(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()
	selector := mustParse(t, enrol(t, store, "alpha", "U-alpha", time.Time{})).Selector

	spy := &closerSpy{err: errors.New("socket already gone")}
	record, found, err := Revoker{Store: store, Connections: spy}.Revoke(ctx, selector, now)
	if err == nil {
		t.Fatal("a failure to close live connections was swallowed")
	}
	if !found || record.RevokedAt.IsZero() {
		t.Fatal("the revocation itself must stand and be reported")
	}
	stored, _, _ := store.BySelector(ctx, selector)
	if stored.RevokedAt.IsZero() {
		t.Fatal("the credential was not left revoked in the store")
	}
}

// TestRevokerWithNoConnectionOwnerStillRevokes is stage 1's actual
// configuration: nothing implements ConnectionCloser yet.
func TestRevokerWithNoConnectionOwnerStillRevokes(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()
	token := enrol(t, store, "alpha", "U-alpha", time.Time{})

	if _, _, err := (Revoker{Store: store}).Revoke(ctx, mustParse(t, token).Selector, now); err != nil {
		t.Fatalf("Revoke with a nil closer: %v", err)
	}
	if _, err := Verify(ctx, store, token, now); !errors.Is(err, ErrRevoked) {
		t.Fatalf("the credential still verifies after revocation: %v", err)
	}
}

func mustParse(t *testing.T, token string) Credential {
	t.Helper()
	credential, err := Parse(token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return credential
}

// flipLastChar changes the final character of a base64url secret to a different
// legal one, so the tampered token is still well-formed and the rejection has to
// come from the secret comparison rather than from Parse.
func flipLastChar(s string) string {
	last := s[len(s)-1]
	replacement := byte('A')
	if last == 'A' {
		replacement = 'B'
	}
	return s[:len(s)-1] + string(replacement)
}
