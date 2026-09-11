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

// Not a secret: it lets scanners and log greps spot a leaked token, and
// internal/troubleshoot redacts on it.
const Prefix = "mrtg_node_"

const selectorBytes = 8

const secretBytes = 32

const selectorLen = selectorBytes * 2

// A named type so the nodetokenanalyzer arch guard can reject == on it, which
// keeps Equal constant-time.
type Digest string

// None of these quotes the input: an error string is where a rejected
// credential most reliably reaches a log.
var (
	ErrMalformed = errors.New("node token: malformed")
)

// Holds the plaintext secret, so it has no String or MarshalJSON on purpose:
// either would leak the secret into logs.
type Credential struct {
	Selector string
	Secret   string
}

type Minted struct {
	// The plaintext exists only here; it cannot be recovered from the store.
	Token      string
	Selector   string
	SecretHash Digest
}

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

// Malformed input must never reach the store, or an unauthenticated peer gets
// a free database query per packet.
func Parse(presented string) (Credential, error) {
	rest, ok := strings.CutPrefix(presented, Prefix)
	if !ok {
		return Credential{}, fmt.Errorf("%w: missing the %s prefix", ErrMalformed, Prefix)
	}
	selector, secret, ok := strings.Cut(rest, "_")
	if !ok {
		return Credential{}, fmt.Errorf("%w: no selector/secret separator", ErrMalformed)
	}
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

// Unsalted on purpose: a 256-bit random secret has nothing to precompute, and
// a salt would break the indexed lookup.
func HashSecret(secret string) Digest {
	sum := sha256.Sum256([]byte(secret))
	return Digest(hex.EncodeToString(sum[:]))
}

// Constant time because verification is unauthenticated; an early-exit compare
// leaks how much of a guess was right.
func Equal(a, b Digest) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
