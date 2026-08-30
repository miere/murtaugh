package claudecode

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsAuthFailureRecognisesRealCLIMessages(t *testing.T) {
	cases := map[string]string{
		"401 invalid api key": "API Error: 401 Invalid API key · Please run /login",
		"oauth 401 on env token": "OAuth 401: keeping the user-supplied CLAUDE_CODE_OAUTH_TOKEN instead of " +
			"adopting the stored credential. Mint a fresh token with `claude setup-token` and restart with it",
		// The signature of a sandbox-blocked write: the server rotated the refresh
		// token, the client could not persist it, and the retired one is rejected.
		"retired refresh token": `token exchange failed: {"error":"invalid_grant"}`,
		"no oauth session":      "Not logged in",
		"authentication_error":  `{"type":"error","error":{"type":"authentication_error","message":"x"}}`,
		"run login bare":        "Run /login to sign in with your claude.ai account",
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			if !IsAuthFailure(errors.New(msg)) {
				t.Fatalf("expected %q to be classified as an auth failure", msg)
			}
		})
	}
}

// A wrapped error must still classify: the launch path wraps the child's stderr
// several layers deep before the gateway sees it.
func TestIsAuthFailureSeesThroughWrapping(t *testing.T) {
	inner := errors.New("API Error: 401 Invalid API key · Please run /login")
	wrapped := fmt.Errorf("claudecode: start session abc: resume=%v; create=%w", inner, inner)
	if !IsAuthFailure(wrapped) {
		t.Fatal("expected a wrapped auth failure to be classified")
	}
}

// A false positive nags the admin to repair a credential that was fine, which is
// worse than leaving the user with the raw error. These must NOT match.
func TestIsAuthFailureIgnoresOrdinaryFailures(t *testing.T) {
	for name, msg := range map[string]string{
		"nil":              "",
		"tool ceiling":     "claudecode: turn aborted mid-execution (error_max_turns)",
		"stream closed":    "claudecode: stream closed",
		"process missing":  `claudecode: start "claude": exec: "claude": executable file not found in $PATH`,
		"rate limited":     "API Error: 429 rate limited",
		"overloaded":       "API Error: 529 overloaded",
		"usage limit":      "usage limit reached",
		"unrelated 401":    "the vaultre MCP server returned 401 for /properties",
		"someone said gcp": "gcloud auth login is required for the gcp-mcp server",
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			if msg != "" {
				err = errors.New(msg)
			}
			if IsAuthFailure(err) {
				t.Fatalf("expected %q NOT to be classified as a Claude Code auth failure", msg)
			}
		})
	}
}

func TestIsAuthFailureNilIsFalse(t *testing.T) {
	if IsAuthFailure(nil) {
		t.Fatal("nil must not classify as an auth failure")
	}
}

func TestMentionsAuthFailureIsCaseInsensitive(t *testing.T) {
	if !mentionsAuthFailure("PLEASE RUN /LOGIN") {
		t.Fatal("classification must not depend on the CLI's capitalisation")
	}
}
