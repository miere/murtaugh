package nodetoken

import (
	"errors"
	"strings"
	"testing"
)

// TestMintedTokensCarryTheGreppablePrefix pins the property a secret scanner and
// a log grep both rely on. Without it a leaked token is an anonymous 70-character
// string that nothing can recognise.
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

// TestMintIsUniqueAndHighEntropy checks the two things a bearer token cannot do
// without: never repeat, and carry enough randomness that guessing is hopeless.
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
		// 32 raw bytes is 43 base64url characters. Asserted rather than assumed:
		// a shortened secret is the one bug here that nothing else would catch.
		if len(credential.Secret) != 43 {
			t.Fatalf("secret is %d characters, want 43 (256 bits)", len(credential.Secret))
		}
	}
}

// TestMintedTokenSelfDescribes checks that what Mint hands the caller to store
// is what verification will later derive from the token itself.
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
	// The plaintext must not be recoverable from what gets stored.
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

// TestParseErrorsDoNotQuoteTheInput: an error string is the one value on this
// path that reliably reaches a log file and a troubleshoot bundle, and a
// rejected credential is still a credential — a near-miss during a rotation, or
// the real token typed at the wrong gateway.
func TestParseErrorsDoNotQuoteTheInput(t *testing.T) {
	minted, err := Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	credential, err := Parse(minted.Token)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Each of these fails a different branch of Parse, so every rejection path
	// is checked rather than just the first.
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

// TestEqual covers the verdicts, which is all a behavioural test of a
// constant-time compare can honestly cover. That the comparison is actually
// constant-time is enforced statically instead, by
// internal/archtest/nodetokenanalyzer — a timing test would be flaky and would
// not check what its name promised.
func TestEqual(t *testing.T) {
	a := HashSecret("alpha")
	b := HashSecret("beta")

	if !Equal(a, HashSecret("alpha")) {
		t.Error("Equal said two digests of the same secret differ")
	}
	if Equal(a, b) {
		t.Error("Equal said two digests of different secrets match")
	}
	// Same length, differing in the last character only — the case a truncated
	// comparison would get wrong.
	nearly := Digest(string(a)[:len(a)-1] + flipHexDigit(string(a)[len(a)-1]))
	if Equal(a, nearly) {
		t.Error("Equal matched digests differing only in the final character")
	}
	// Different lengths.
	if Equal(a, Digest(string(a)[:10])) {
		t.Error("Equal matched a digest against its own prefix")
	}
	if Equal(a, "") {
		t.Error("Equal matched a digest against the empty string")
	}
}

// flipHexDigit returns a different hex digit, so a "nearly equal" digest stays a
// legal digest.
func flipHexDigit(c byte) string {
	if c == '0' {
		return "1"
	}
	return "0"
}

// Note the use of Equal rather than == throughout: the arch guard applies to
// this file too, which is deliberate. A test that compared digests directly
// would be a working example of the mistake sitting next to the rule.
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
