package troubleshoot

import "regexp"

// RedactionLimitations is the honest disclaimer recorded in the manifest and
// shown to whoever opens a bundle. Redaction scrubs the credentials Murtaugh
// recognises — it cannot scrub secrets that only the user knows the shape of
// (API keys pasted into a chat, tokens inside a provider's transcript DB, or
// credentials carried in ACP traffic). Treat a bundle as sensitive regardless.
const RedactionLimitations = "Redaction removes Slack tokens (xoxb-/xapp-/xoxp-/xoxa-/xoxe-/xoxr-) and the " +
	"values of obviously-secret YAML keys (app_token, bot_token, *secret*, *api[_-]?key*, password, " +
	"*credential*). It does NOT and cannot remove credentials embedded in conversation transcripts, " +
	"binary session databases (*.db), or arbitrary strings whose secrecy Murtaugh has no way to know. " +
	"Treat this bundle as sensitive."

// redactedToken is the placeholder substituted for a scrubbed value.
const redactedToken = "‹redacted›"

// slackTokenPattern matches the Slack token families Slack issues. Tokens are
// scrubbed wherever they appear (config values, command args, log lines), which
// is the backstop for secrets sitting under keys we don't recognise.
var slackTokenPattern = regexp.MustCompile(`xox[bpaerso]-[A-Za-z0-9-]{6,}|xapp-[A-Za-z0-9-]{6,}`)

// secretKeyPattern matches a YAML "key: value" line whose key names something
// secret. Group 1 is the "  key: " prefix (preserved); the value is replaced.
// Anchored to a line and tolerant of indentation and optional quoting.
var secretKeyPattern = regexp.MustCompile(
	`(?im)^(\s*[\w.-]*(?:app_token|bot_token|access_token|refresh_token|secret|api[_-]?key|apikey|password|passwd|credentials?|client_secret)[\w.-]*\s*:\s*).+$`)

// redactText scrubs known secrets from a text payload (config files, provider
// YAML/JSON/log files). It is deliberately conservative about what it treats as
// a secret key but greedy about Slack token shapes, since over-redaction is
// safe in a diagnostics bundle. Returns the redacted text and whether anything
// changed.
func redactText(in []byte) (out []byte, changed bool) {
	redacted := secretKeyPattern.ReplaceAll(in, []byte("${1}"+redactedToken))
	redacted = slackTokenPattern.ReplaceAll(redacted, []byte(redactedToken))
	return redacted, len(redacted) != len(in) || !equalBytes(redacted, in)
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
