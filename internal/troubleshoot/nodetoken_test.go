package troubleshoot

import (
	"strings"
	"testing"

	"github.com/miere/murtaugh/internal/nodetoken"
)

// nodeTokenPattern hard-codes the prefix instead of importing nodetoken, so only this
// test catches the two drifting apart and bundles leaking live tokens.
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
		if !contains(out, "a log line carrying ") || !contains(out, " in the clear") {
			t.Fatalf("redaction consumed the surrounding text:\n%s", out)
		}
	}
}

// A token reaches a log truncated more often than whole (a wrapped shell line, a paste
// that lost its tail), so the selector alone must be scrubbed too.
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

// Operators judge how far to trust a bundle from this note, so it must name every kind
// of token the code actually scrubs.
func TestRedactionLimitationsNamesNodeTokens(t *testing.T) {
	if !strings.Contains(RedactionLimitations, nodetoken.Prefix) {
		t.Fatalf("RedactionLimitations does not mention %q:\n%s", nodetoken.Prefix, RedactionLimitations)
	}
}
