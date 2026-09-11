package nodetoken

import (
	"errors"
	"strings"
	"testing"
)

func TestMintedTokensCarryTheGreppablePrefix(t *testing.T) {
	minted, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(minted.Token, "mrtg_node_") {
		t.Fatalf("token %q does not start with mrtg_node_", minted.Token)
	}
	if Prefix != "mrtg_node_" {
		t.Fatalf("Prefix = %q; the spec (#190) names mrtg_node_ and secret scanners will be configured for it", Prefix)
	}
}

func TestMintIsUniqueAndHighEntropy(t *testing.T) {
	const runs = 200
	selectors := make(map[string]bool, runs)
	tokens := make(map[string]bool, runs)
	for i := 0; i < runs; i++ {
		minted, err := Mint()
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if selectors[minted.Selector] {
			t.Fatalf("Mint repeated a selector after %d draws", i)
		}
		if tokens[minted.Token] {
			t.Fatalf("Mint repeated a token after %d draws", i)
		}
		selectors[minted.Selector] = true
		tokens[minted.Token] = true

		credential, err := Parse(minted.Token)
		if err != nil {
			t.Fatalf("a freshly minted token did not parse: %v", err)
		}
		if len(credential.Secret) != 43 {
			t.Fatalf("secret is %d characters, want 43 (256 bits)", len(credential.Secret))
		}
	}
}

func TestMintedTokenSelfDescribes(t *testing.T) {
	minted, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	credential, err := Parse(minted.Token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if credential.Selector != minted.Selector {
		t.Errorf("parsed selector %q, want %q", credential.Selector, minted.Selector)
	}
	if !Equal(HashSecret(credential.Secret), minted.SecretHash) {
		t.Error("the digest of the token's own secret does not match the one Mint returned to store")
	}
	if strings.Contains(string(minted.SecretHash), credential.Secret) {
		t.Error("the stored digest contains the secret")
	}
}

func TestParseRejectsMalformedTokens(t *testing.T) {
	minted, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	credential, err := Parse(minted.Token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	cases := map[string]string{
		"empty":                 "",
		"no prefix":             credential.Selector + "_" + credential.Secret,
		"wrong prefix":          "mrtg_user_" + credential.Selector + "_" + credential.Secret,
		"prefix only":           Prefix,
		"no separator":          Prefix + credential.Selector + credential.Secret[:0],
		"short selector":        Prefix + credential.Selector[:8] + "_" + credential.Secret,
		"long selector":         Prefix + credential.Selector + "ab" + "_" + credential.Secret,
		"non-hex selector":      Prefix + strings.Repeat("z", len(credential.Selector)) + "_" + credential.Secret,
		"empty secret":          Prefix + credential.Selector + "_",
		"a slack token":         "xoxb-123456789012-abcdefghijkl",
		"leading whitespace":    " " + minted.Token,
		"selector without hash": Prefix + credential.Selector,
	}
	for name, presented := range cases {
		if _, err := Parse(presented); err == nil {
			t.Errorf("Parse accepted %s: %q", name, presented)
		} else if !errors.Is(err, ErrMalformed) {
			t.Errorf("Parse(%s) = %v, want an ErrMalformed", name, err)
		}
	}
}

func TestParseErrorsDoNotQuoteTheInput(t *testing.T) {
	minted, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	credential, err := Parse(minted.Token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	for name, presented := range map[string]string{
		"no prefix":        credential.Selector + "_" + credential.Secret,
		"no separator":     Prefix + credential.Selector,
		"short selector":   Prefix + credential.Selector[:8] + "_" + credential.Secret,
		"non-hex selector": Prefix + strings.Repeat("z", len(credential.Selector)) + "_" + credential.Secret,
		"empty secret":     Prefix + credential.Selector + "_",
	} {
		_, err := Parse(presented)
		if err == nil {
			t.Fatalf("%s was accepted", name)
		}
		if strings.Contains(err.Error(), credential.Secret) {
			t.Errorf("the %s error quotes the presented secret: %v", name, err)
		}
		if strings.Contains(err.Error(), presented) {
			t.Errorf("the %s error quotes the whole presented token: %v", name, err)
		}
	}
}

// Only the verdicts: constant time is enforced by the nodetokenanalyzer arch
// guard, because a timing test would be flaky.
func TestEqual(t *testing.T) {
	a := HashSecret("alpha")
	b := HashSecret("beta")

	if !Equal(a, HashSecret("alpha")) {
		t.Error("Equal said two digests of the same secret differ")
	}
	if Equal(a, b) {
		t.Error("Equal said two digests of different secrets match")
	}
	nearly := Digest(string(a)[:len(a)-1] + flipHexDigit(string(a)[len(a)-1]))
	if Equal(a, nearly) {
		t.Error("Equal matched digests differing only in the final character")
	}
	if Equal(a, Digest(string(a)[:10])) {
		t.Error("Equal matched a digest against its own prefix")
	}
	if Equal(a, "") {
		t.Error("Equal matched a digest against the empty string")
	}
}

func flipHexDigit(c byte) string {
	if c == '0' {
		return "1"
	}
	return "0"
}

// Uses Equal, not ==, on purpose: the arch guard covers this file too.
func TestHashSecretIsStableAndDistinct(t *testing.T) {
	if !Equal(HashSecret("secret"), HashSecret("secret")) {
		t.Error("HashSecret is not deterministic; a stored digest would stop matching")
	}
	if Equal(HashSecret("secret"), HashSecret("secrer")) {
		t.Error("HashSecret collided on two one-character-apart inputs")
	}
	if got := len(HashSecret("secret")); got != 64 {
		t.Errorf("digest is %d characters, want 64 (hex SHA-256)", got)
	}
}
