package auth

import "testing"

// The exact line CLI 2.1.238 prints to a plain pipe. Kept verbatim (including
// the redirect_uri that also points at a claude.com-adjacent host) because the
// whole risk in this profile is picking the wrong URL out of a real line.
const realLoginLine = `If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile+user%3Ainference&code_challenge=mZioQLsnrQWX7owYtUe_kW-JQJc1iItirISM4gUlCpo&code_challenge_method=S256&state=y_pucGhaVzjF97wrrPRD5WeTCoR6vFh6jKV3LFJIRiA`

func TestClaudeCodeProfileIsRegistered(t *testing.T) {
	p, ok := Lookup("claude-code")
	if !ok {
		t.Fatal("claude-code profile is not registered")
	}
	if !p.NeedsCode {
		t.Fatal("claude-code must be a code flow: the CLI prompts for a pasted code")
	}
	if p.Command != "claude" {
		t.Fatalf("command = %q, want claude", p.Command)
	}
	// --claudeai is load-bearing: without it the CLI opens an interactive
	// "select login method" menu that a headless pipe cannot navigate, and the
	// flow never reaches the URL.
	var sawClaudeAI bool
	for _, a := range p.Args {
		if a == "--claudeai" {
			sawClaudeAI = true
		}
	}
	if !sawClaudeAI {
		t.Fatalf("args %v must include --claudeai to skip the interactive method menu", p.Args)
	}
	// setup-token would also fit this card, and is deliberately not used: it
	// mints an ~annual, inference-only token instead of the ordinary rotating
	// credential.
	for _, a := range p.Args {
		if a == "setup-token" {
			t.Fatal("claude-code must drive `auth login`, not `setup-token`")
		}
	}
}

func TestClaudeCodeExtractsConsentURL(t *testing.T) {
	p, _ := Lookup("claude-code")
	got, ok := p.ExtractURL(realLoginLine)
	if !ok {
		t.Fatal("failed to extract the consent URL from the real CLI output line")
	}
	const want = `https://claude.com/cai/oauth/authorize?code=true&client_id=9d1c250a-e61b-44d9-88ed-5944d1962f5e&response_type=code&redirect_uri=https%3A%2F%2Fplatform.claude.com%2Foauth%2Fcode%2Fcallback&scope=org%3Acreate_api_key+user%3Aprofile+user%3Ainference&code_challenge=mZioQLsnrQWX7owYtUe_kW-JQJc1iItirISM4gUlCpo&code_challenge_method=S256&state=y_pucGhaVzjF97wrrPRD5WeTCoR6vFh6jKV3LFJIRiA`
	if got != want {
		t.Fatalf("extracted URL\n got: %s\nwant: %s", got, want)
	}
}

// The consent URL and the callback URL live on the same line. Handing the admin
// the callback would send them to a page that cannot authorise anything.
func TestClaudeCodeDoesNotMatchTheCallbackURL(t *testing.T) {
	p, _ := Lookup("claude-code")
	got, ok := p.ExtractURL(`redirect_uri is https://platform.claude.com/oauth/code/callback for this flow`)
	if ok {
		t.Fatalf("matched a non-consent URL: %s", got)
	}
}

func TestClaudeCodeIgnoresUnrelatedURLs(t *testing.T) {
	p, _ := Lookup("claude-code")
	for name, line := range map[string]string{
		"docs link":  "See https://code.claude.com/docs/en/overview for help",
		"issues":     "report the issue at https://github.com/anthropics/claude-code/issues",
		"google":     "https://accounts.google.com/o/oauth2/auth?client_id=x",
		"plain text": "Opening browser to sign in…",
	} {
		t.Run(name, func(t *testing.T) {
			if got, ok := p.ExtractURL(line); ok {
				t.Fatalf("unexpectedly matched %q -> %s", line, got)
			}
		})
	}
}

func TestClaudeCodeIsSelectableByName(t *testing.T) {
	p, err := Resolve("claude-code", "", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Name != "claude-code" {
		t.Fatalf("name = %q", p.Name)
	}
	// A built-in runs a fixed command; passing one alongside must stay an error
	// rather than silently running the caller's.
	if _, err := Resolve("claude-code", "rm -rf /", false); err == nil {
		t.Fatal("expected a command alongside a built-in profile to be rejected")
	}
}

func TestClaudeCodeAppearsInNames(t *testing.T) {
	var found bool
	for _, n := range Names() {
		if n == "claude-code" {
			found = true
		}
	}
	if !found {
		t.Fatalf("claude-code missing from Names(): %v", Names())
	}
}
