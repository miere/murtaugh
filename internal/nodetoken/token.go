package nodetoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Prefix marks a Murtaugh node token. It is not a secret and carries no
// entropy; it exists so a token that escapes into a log, a shell history or a
// pasted transcript is greppable, and so a secret scanner has a shape to match.
// internal/troubleshoot redacts on it.
const Prefix = "mrtg_node_"

// selectorBytes is the credential id's length. It is a lookup key, not a
// secret, so it only has to be collision-free across a fleet's worth of
// credentials; 64 bits is far past that.
const selectorBytes = 8

// secretBytes is the token's actual entropy. 256 random bits is why a plain
// SHA-256 is enough at rest: there is nothing to guess and no dictionary to
// walk, so a password-grade KDF would slow every handshake down to buy nothing.
const secretBytes = 32

// selectorLen is the selector's encoded width. Fixed, because a verifier that
// accepted a short selector would accept a truncated token.
const selectorLen = selectorBytes * 2 // lowercase hex

// Digest is the SHA-256 of a token secret, lowercase hex.
//
// It is a named type rather than a string for a mechanical reason: the
// nodetokenanalyzer arch guard (run by cmd/archcheck) reports any `==` or `!=`
// whose operand is a Digest, so the constant-time comparison in Equal cannot be
// "simplified" back into an ordinary string compare without failing the build.
type Digest string

// Errors a presented token can fail with before the store is ever consulted.
// None of them quotes the input: an error string is the one place a rejected
// credential reliably reaches a log.
var (
	// ErrMalformed means the presented string is not shaped like a node token.
	ErrMalformed = errors.New("node token: malformed")
)

// Credential is a parsed token: the public part used to find the record, and
// the secret part used to prove it.
//
// It holds the plaintext secret and is therefore short-lived by construction —
// Parse returns it, Verify consumes it, nothing stores it. It has no String or
// MarshalJSON on purpose: adding one would put the secret in the first `%v` or
// log line that touched it.
type Credential struct {
	// Selector is the credential id. Not a secret; it is the store key.
	Selector string
	// Secret is the presented plaintext secret. Never stored, never logged.
	Secret string
}

// Minted is a freshly created credential: the plaintext to hand to the node,
// plus the two values that are safe to persist.
//
// The plaintext exists in this struct and nowhere else in the package. Callers
// deliver it once and drop it.
type Minted struct {
	// Token is the full `mrtg_node_…` string. This is the only moment it
	// exists; it cannot be recovered from the store afterwards.
	Token string
	// Selector is the credential id to store and to name in `node token revoke`.
	Selector string
	// SecretHash is what gets persisted in place of the token.
	SecretHash Digest
}

// Mint creates a new credential from crypto/rand.
//
// It does not touch the store and knows nothing about which node or user the
// credential is for. That binding is the store record's job (config.NodeToken),
// which keeps this function trivially testable and keeps the identity lookup in
// one place.
func Mint() (Minted, error) {
	selector := make([]byte, selectorBytes)
	if _, err := rand.Read(selector); err != nil {
		return Minted{}, fmt.Errorf("node token: read random selector: %w", err)
	}
	secret := make([]byte, secretBytes)
	if _, err := rand.Read(secret); err != nil {
		return Minted{}, fmt.Errorf("node token: read random secret: %w", err)
	}

	sel := hex.EncodeToString(selector)
	sec := base64.RawURLEncoding.EncodeToString(secret)
	return Minted{
		Token:      Prefix + sel + "_" + sec,
		Selector:   sel,
		SecretHash: HashSecret(sec),
	}, nil
}

// Parse splits a presented token into its selector and secret, rejecting
// anything that is not shaped like one.
//
// Every rejection here happens BEFORE the store is consulted, which is the
// point: a malformed string must not become a database round trip, or an
// unauthenticated peer gets a free query per packet.
func Parse(presented string) (Credential, error) {
	rest, ok := strings.CutPrefix(presented, Prefix)
	if !ok {
		return Credential{}, fmt.Errorf("%w: missing the %s prefix", ErrMalformed, Prefix)
	}
	selector, secret, ok := strings.Cut(rest, "_")
	if !ok {
		return Credential{}, fmt.Errorf("%w: no selector/secret separator", ErrMalformed)
	}
	// The selector is fixed-width lowercase hex, so it never contains the
	// separator itself and the Cut above cannot split in the wrong place even
	// though the secret's base64url alphabet does include '_'.
	if len(selector) != selectorLen {
		return Credential{}, fmt.Errorf("%w: selector is %d characters, want %d", ErrMalformed, len(selector), selectorLen)
	}
	if _, err := hex.DecodeString(selector); err != nil {
		return Credential{}, fmt.Errorf("%w: selector is not hex", ErrMalformed)
	}
	if secret == "" {
		return Credential{}, fmt.Errorf("%w: empty secret", ErrMalformed)
	}
	return Credential{Selector: selector, Secret: secret}, nil
}

// HashSecret returns the digest stored in place of a token secret.
//
// Unsalted, deliberately. A salt defends a low-entropy password against
// precomputation; a 256-bit random secret has no precomputable space, so a
// per-credential salt would buy nothing and would cost the indexed lookup that
// makes verification a single round trip.
func HashSecret(secret string) Digest {
	sum := sha256.Sum256([]byte(secret))
	return Digest(hex.EncodeToString(sum[:]))
}

// Equal reports whether two digests match, in time independent of how many
// leading characters they share.
//
// Verification is an unauthenticated path: the caller is by definition someone
// who may be guessing. A byte-wise comparison that returns early leaks how much
// of a guess was right, which turns an unguessable secret into a per-character
// search.
//
// crypto/subtle.ConstantTimeCompare returns 0 for differing lengths. That is
// fine here — both operands are fixed-width hex digests of the caller's input,
// so their length reveals nothing about the stored secret.
func Equal(a, b Digest) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
