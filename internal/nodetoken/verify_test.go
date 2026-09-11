package nodetoken

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/miere/murtaugh/internal/config"
)

type memStore struct {
	records  map[string]config.NodeToken
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

	beta, err := Verify(ctx, store, betaToken, now)
	if err != nil {
		t.Fatalf("Verify(beta): %v", err)
	}
	if beta.NodeID != "beta" || beta.UserID != "U-beta" {
		t.Fatalf("beta's token resolved to node %q user %q", beta.NodeID, beta.UserID)
	}
}

func TestVerifyRejectsATamperedToken(t *testing.T) {
	store := newMemStore()
	ctx := context.Background()
	token := enrol(t, store, "alpha", "U-alpha", time.Time{})
	credential, err := Parse(token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	tampered := Prefix + credential.Selector + "_" + flipLastChar(credential.Secret)
	if _, err := Verify(ctx, store, tampered, now); !errors.Is(err, ErrSecretMismatch) {
		t.Fatalf("Verify(tampered secret) = %v, want ErrSecretMismatch", err)
	}
	if _, err := Verify(ctx, store, tampered, now); !errors.Is(err, ErrNotAuthorized) {
		t.Fatal("a rejection must satisfy errors.Is(err, ErrNotAuthorized) so one check covers the handshake path")
	}

	unknown := Prefix + strings.Repeat("ab", 8) + "_" + credential.Secret
	if _, err := Verify(ctx, store, unknown, now); !errors.Is(err, ErrUnknownCredential) {
		t.Fatalf("Verify(unknown selector) = %v, want ErrUnknownCredential", err)
	}
}

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

func TestRecheckRejectsWhatVerifyWouldRejectOnceTheSecretIsProven(t *testing.T) {
	ctx := context.Background()
	expiry := now.Add(time.Hour)
	store := newMemStore()
	live := mustParse(t, enrol(t, store, "alpha", "U-alpha", expiry)).Selector
	revoked := mustParse(t, enrol(t, store, "alpha", "U-alpha", time.Time{})).Selector
	if _, _, err := store.Revoke(ctx, revoked, now); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		selector string
		at       time.Time
		want     error
	}{
		"live":    {selector: live, at: now},
		"revoked": {selector: revoked, at: now, want: ErrRevoked},
		"expired": {selector: live, at: expiry, want: ErrExpired},
		"gone":    {selector: "ffffffffffffffff", at: now, want: ErrUnknownCredential},
	} {
		t.Run(name, func(t *testing.T) {
			err := Recheck(ctx, store, tc.selector, tc.at)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Recheck(%s) = %v, want nil", name, err)
				}
				return
			}
			if !errors.Is(err, tc.want) || !errors.Is(err, ErrNotAuthorized) {
				t.Fatalf("Recheck(%s) = %v, want %v wrapping ErrNotAuthorized", name, err, tc.want)
			}
		})
	}
}

// A connection dropped over a database hiccup would take the whole fleet down
// with the database.
func TestRecheckDoesNotTreatAStoreFailureAsARejection(t *testing.T) {
	store := newMemStore()
	selector := mustParse(t, enrol(t, store, "alpha", "U-alpha", time.Time{})).Selector
	boom := errors.New("connection refused")
	store.failWith = boom

	err := Recheck(context.Background(), store, selector, now)
	if !errors.Is(err, boom) {
		t.Fatalf("Recheck = %v, want the store's own error", err)
	}
	if errors.Is(err, ErrNotAuthorized) {
		t.Fatal("a store outage was reported as a failed authorisation")
	}
}

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

type closerSpy struct {
	closed []string
	err    error
}

func (c *closerSpy) CloseCredential(_ context.Context, selector string) error {
	c.closed = append(c.closed, selector)
	return c.err
}

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

// An unknown selector is a typo or a probe, and must not be a way to disturb
// live sessions.
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

// The revocation is already durable, so a close failure is reported rather
// than treated as a failed revoke.
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

func flipLastChar(s string) string {
	last := s[len(s)-1]
	replacement := byte('A')
	if last == 'A' {
		replacement = 'B'
	}
	return s[:len(s)-1] + string(replacement)
}
