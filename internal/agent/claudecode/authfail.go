package claudecode

import (
	"errors"
	"strings"
)

// authFailureMarkers are the substrings that identify a Claude Code failure as
// "the credential is bad" rather than "the turn went wrong".
//
// They are matched against the error text, which for a failed launch already
// carries the child's stderr (see procSession.stderrSuffix). Every entry is a
// string the CLI itself emits; they are grouped by what produced them so a
// future CLI change can be traced back to a source rather than guessed at.
//
// Matching on prose is unavoidable here — the stream-json protocol has no typed
// auth-failure signal — so the list is deliberately SPECIFIC. A false positive
// nags the admin to re-authenticate a credential that was fine, which is far
// worse than a false negative: the latter merely leaves the user with the raw
// error they would have seen anyway.
var authFailureMarkers = []string{
	// The canonical runtime rejection: "API Error: 401 Invalid API key · Please
	// run /login".
	"please run /login",
	"run /login",
	// Emitted when a user-supplied CLAUDE_CODE_OAUTH_TOKEN is rejected and the
	// CLI declines to fall back to the stored credential.
	"oauth 401",
	// The OAuth server's answer when a refresh token has been retired — which is
	// exactly what a sandbox-blocked write leaves behind, since the server rotates
	// the token even though the client could not persist the new one.
	"invalid_grant",
	// Admin/API surfaces when no OAuth session exists at all.
	"not logged in",
	"invalid api key",
	"authentication_error",
}

// IsAuthFailure reports whether err describes a Claude Code failure that a
// re-authentication would fix.
//
// It exists so the gateway can tell the two apart without parsing errors at the
// call site: an ordinary turn failure is the user's problem to read, while a
// credential failure is the admin's to repair and the user can do nothing about
// it. Being wrong in the permissive direction costs an unnecessary admin prompt,
// so the marker list is kept narrow.
func IsAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	return mentionsAuthFailure(err.Error())
}

// mentionsAuthFailure is the string half, split out so it can be applied to
// output that never became an error (a `result` payload carrying the CLI's
// message) and tested directly.
func mentionsAuthFailure(text string) bool {
	lowered := strings.ToLower(text)
	for _, marker := range authFailureMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// ErrCredentialRejected wraps an underlying failure the gateway has classified
// as a credential problem, so downstream code can branch on it with errors.Is
// rather than re-running the string match.
var ErrCredentialRejected = errors.New("claudecode: credential rejected")
