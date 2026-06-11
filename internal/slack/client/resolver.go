package client

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// ResolveTarget resolves a `--to`-style value into a Slack channel ID.
// Accepted forms:
//
//	#channel-name              -> ResolveChannel
//	@user-handle               -> ResolveUser, then OpenDM
//	C..., G..., D...           -> pass through as-is (channel/group/DM IDs)
//
// Raw user IDs (U...) are rejected. Callers that already have a user ID
// should call OpenDM directly and pass the resulting D... ID here.
func ResolveTarget(ctx context.Context, api SlackAPI, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("Error: --to is required")
	}

	switch {
	case strings.HasPrefix(target, "#"):
		return ResolveChannel(ctx, api, target)
	case strings.HasPrefix(target, "@"):
		userID, err := ResolveUser(ctx, api, target)
		if err != nil {
			return "", err
		}
		return api.OpenDM(ctx, userID)
	case startsWithAny(target, "C", "G", "D"):
		return target, nil
	default:
		return "", fmt.Errorf("Error: --to must start with # (channel), @ (user), or be a raw Slack ID.")
	}
}

// ResolveChannel strips any leading "#", then pages through
// conversations.list and returns the channel whose name OR id matches the
// input.
func ResolveChannel(ctx context.Context, api SlackAPI, name string) (string, error) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "#")
	channels, err := api.ListChannels(ctx)
	if err != nil {
		return "", err
	}
	for _, ch := range channels {
		if ch.Name == name || ch.ID == name {
			return ch.ID, nil
		}
	}
	return "", fmt.Errorf("Channel '%s' not found.", name)
}

// ResolveUser strips any leading "@", lowercases the handle, then checks
// legacy username → display name → real name in that order. Match is
// case-insensitive.
func ResolveUser(ctx context.Context, api SlackAPI, handle string) (string, error) {
	handle = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(handle), "@"))
	users, err := api.ListUsers(ctx)
	if err != nil {
		return "", err
	}
	for _, u := range users {
		if strings.ToLower(u.Name) == handle {
			return u.ID, nil
		}
	}
	for _, u := range users {
		if strings.ToLower(u.DisplayName) == handle {
			return u.ID, nil
		}
	}
	for _, u := range users {
		if strings.ToLower(u.RealName) == handle {
			return u.ID, nil
		}
	}
	return "", fmt.Errorf("User '%s' not found.", handle)
}

// mentionPattern matches @handle. The "@ not preceded by a word character"
// rule is enforced manually in ResolveMentions because Go's regexp engine
// has no lookbehind.
var mentionPattern = regexp.MustCompile(`@([a-zA-Z0-9._-]+)`)

// ResolveMentions replaces every @handle in text with <@USER_ID> Slack
// syntax. A handle directly preceded by a word character (letter, digit,
// underscore) is left untouched. Unresolvable handles are left as-is and a
// warning is written to warn (typically os.Stderr); pass io.Discard to
// suppress warnings.
func ResolveMentions(ctx context.Context, api SlackAPI, text string, warn io.Writer) string {
	matches := mentionPattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	cursor := 0
	for _, m := range matches {
		start, end, handleStart, handleEnd := m[0], m[1], m[2], m[3]
		b.WriteString(text[cursor:start])
		if start > 0 && isWordByte(text[start-1]) {
			b.WriteString(text[start:end])
			cursor = end
			continue
		}
		handle := text[handleStart:handleEnd]
		userID, err := ResolveUser(ctx, api, handle)
		if err != nil {
			fmt.Fprintf(warn, "Warning: user '@%s' not found, leaving as plain text.\n", handle)
			b.WriteString(text[start:end])
		} else {
			b.WriteString("<@")
			b.WriteString(userID)
			b.WriteString(">")
		}
		cursor = end
	}
	b.WriteString(text[cursor:])
	return b.String()
}

func isWordByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

func startsWithAny(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
