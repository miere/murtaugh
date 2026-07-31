package gateway

import (
	"path"
	"strings"

	"github.com/miere/murtaugh/internal/config"
)

// matchChannel resolves a channel to its configured chat.channels rule. Each
// rule's `match` is an exact Slack channel ID (C…/G…), an exact channel NAME, or
// a channel-NAME glob that may contain `*` (e.g. "feature-*", "*-prod").
//
// Precedence is POSITIONAL: the rules are walked in the order the operator wrote
// them and the FIRST match wins. There is no implicit specificity scoring — a
// narrow rule that must beat a broader one has to be listed above it. This is
// why chat.channels is an ordered list rather than a map: it makes precedence
// something you read off the config instead of deriving.
//
// channelName may be empty when the channel's name could not be resolved; only
// exact channel-ID rules can match in that case.
//
// ok is false (and the zero ChannelConfig is returned) when nothing matches;
// callers fall back to chat.defaults in that case. The winning rule supplies the
// agent, the reply strategy AND the allow_anyone waiver: the caller backfills an
// empty agent from the default and an unset reply_on_thread from the default.
// The function is pure — it does no I/O — so it is safe to call on the Slack
// socket goroutine.
func matchChannel(channelID, channelName string, channels config.ChannelRules) (config.ChannelConfig, bool) {
	for _, cc := range channels {
		if channelKeyMatches(cc.Match, channelID, channelName) {
			return cc, true
		}
	}
	return config.ChannelConfig{}, false
}

// channelAllowsAnyone reports whether the channel's winning chat.channels rule
// opens the chat surface to any workspace user, waiving access.allowed_users.
//
// It reuses matchChannel's first-match-wins semantics rather than unioning
// across every matching rule, and that distinction is a safety property: a
// narrow closed rule listed above a broad `allow_anyone` one keeps its channel
// closed. Unioning would let one permissive glob silently open everything below
// it. No match at all means no waiver, so the global allowlist stands.
func channelAllowsAnyone(channelID, channelName string, channels config.ChannelRules) bool {
	cc, ok := matchChannel(channelID, channelName, channels)
	return ok && cc.AllowAnyone
}

// validChannelAgentGlob reports whether key is a syntactically valid
// chat.channels key. Exact channel-ID/name keys are always valid; glob keys
// (those containing `*`) must be accepted by path.Match so a malformed pattern
// is rejected at config-load time rather than silently never matching.
func validChannelAgentGlob(key string) bool {
	if !strings.ContainsRune(key, '*') {
		return true
	}
	// path.Match only reports ErrBadPattern for malformed patterns; matching
	// against a fixed probe string is enough to surface that error.
	_, err := path.Match(key, "probe")
	return err == nil
}

// usersAllowedWithoutMention returns the effective set of Slack user IDs whose
// plain channel messages the bot replies to WITHOUT an @mention in the channel
// identified by channelID/channelName. Unlike matchChannel's single-winner
// precedence, this is a UNION: the global list plus the users from EVERY
// per-channel pattern whose key matches the channel (an exact channel-ID key, an
// exact channel-name key, or a `*` glob on the name — the same key syntax as
// chat.channels). channelName may be empty when the cache has not yet
// learned the channel's name; only exact channel-ID keys can match in that case.
//
// The result is a set keyed by user ID for O(1) membership tests at the call
// site. It is pure (no I/O) so it is safe to call on the Slack socket goroutine.
// A nil/empty result means no one in this channel is waived from the mention
// requirement.
func usersAllowedWithoutMention(channelID, channelName string, global []string, perChannel map[string][]string) map[string]bool {
	set := make(map[string]bool, len(global))
	for _, u := range global {
		if u != "" {
			set[u] = true
		}
	}
	for key, users := range perChannel {
		if !channelKeyMatches(key, channelID, channelName) {
			continue
		}
		for _, u := range users {
			if u != "" {
				set[u] = true
			}
		}
	}
	return set
}

// channelKeyMatches reports whether a chat.channels-style key matches the
// given channel. The key is an exact channel-ID match, an exact channel-name
// match, or — when it contains `*` — a path.Match glob on the channel name. It
// mirrors matchChannel's matching semantics but without the precedence
// scoring, because the no-mention check unions across every matching key rather
// than picking a single winner.
func channelKeyMatches(key, channelID, channelName string) bool {
	if key == "" {
		return false
	}
	if channelID != "" && key == channelID {
		return true
	}
	if channelName == "" {
		return false
	}
	if !strings.ContainsRune(key, '*') {
		return key == channelName
	}
	matched, err := path.Match(key, channelName)
	return err == nil && matched
}
