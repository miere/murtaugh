package troubleshoot

import (
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/nodetoken"
)

// TestNodeTokenPatternMatchesAMintedToken is the pin the redaction pattern is
// written against. nodeTokenPattern spells the prefix out as a literal rather
// than importing nodetoken (the bundler has no other reason to depend on the
// credential package), so nothing but this test stops the two drifting apart —
// a renamed prefix would otherwise leave a pattern that matches nothing and a
// bundle that carries live credentials, with every other test still green.
//
// It mints a REAL token rather than using a hand-written sample, so the
// alphabet the pattern has to survive (base64url, which includes '-' and '_')
// is the one Mint actually produces.
func TestNodeTokenPatternMatchesAMintedToken(t *testing.T) {
	for i := 0; i < 50; i++ {
		minted, err := nodetoken.Mint()
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		out, changed := redactText([]byte("a log line carrying " + minted.Token + " in the clear\n"))
		if !changed {
			t.Fatalf("redaction left a minted node token untouched: %q", minted.Token)
		}
		if contains(out, minted.Token) {
			t.Fatalf("the token survived redaction:\n%s", out)
		}
		if !contains(out, redactedToken) {
			t.Fatalf("the token was removed without leaving the redaction marker:\n%s", out)
		}
		// The surrounding prose must survive: a pattern greedy enough to eat the
		// line would make bundles useless for the diagnosis they exist for.
		if !contains(out, "a log line carrying ") || !contains(out, " in the clear") {
			t.Fatalf("redaction consumed the surrounding text:\n%s", out)
		}
	}
}

// TestNodeTokenRedactionCoversPartialCopies: a token reaches a log more often
// truncated than whole — a shell that wrapped it, a paste that lost the tail.
// The selector is not itself a secret, but a bundle is the wrong place to be
// precise about that.
func TestNodeTokenRedactionCoversPartialCopies(t *testing.T) {
	minted, err := nodetoken.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	for name, fragment := range map[string]string{
		"selector only":       nodetoken.Prefix + minted.Selector,
		"truncated secret":    minted.Token[:len(minted.Token)-10],
		"quoted in a flag":    `--token "` + minted.Token + `"`,
		"embedded in a field": "token=" + minted.Token + ",node=mac-mini",
	} {
		t.Run(name, func(t *testing.T) {
			out, changed := redactText([]byte(fragment))
			if !changed {
				t.Fatalf("redaction left %q untouched", fragment)
			}
			if contains(out, nodetoken.Prefix+minted.Selector) {
				t.Fatalf("a token-shaped fragment survived redaction:\n%s", out)
			}
		})
	}
}

// TestRedactionLimitationsNamesNodeTokens: the bundle prints this string to tell
// an operator what was and was not scrubbed. A claim there that the code does
// not implement is worse than no claim, and the reverse — scrubbing something
// the note does not mention — leaves an operator over-trusting a redacted file.
func TestRedactionLimitationsNamesNodeTokens(t *testing.T) {
	if !strings.Contains(RedactionLimitations, nodetoken.Prefix) {
		t.Fatalf("RedactionLimitations does not mention %q:\n%s", nodetoken.Prefix, RedactionLimitations)
	}
}
