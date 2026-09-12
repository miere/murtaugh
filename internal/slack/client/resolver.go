package client

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/miere/murtaugh/internal/convref"
)

var slackID = regexp.MustCompile(`^[A-Z][A-Z0-9]*[0-9][A-Z0-9]*$`)

var escapedRef = regexp.MustCompile(`^<([@#])([A-Z][A-Z0-9]+)(?:\|[^>]*)?>$`)

type refKind int

const (
	refChannelName refKind = iota
	refChannelID
	refUserHandle
	refUserID
)

func parseRef(ref string) (refKind, string) {
	ref = strings.TrimSpace(ref)
	if m := escapedRef.FindStringSubmatch(ref); m != nil {
		if m[1] == "@" {
			return refUserID, m[2]
		}
		return refChannelID, m[2]
	}
	if strings.HasPrefix(ref, "@") {
		handle := strings.TrimPrefix(ref, "@")
		if isUserID(handle) {
			return refUserID, handle
		}
		return refUserHandle, handle
	}
	if isUserID(ref) {
		return refUserID, ref
	}
	if slackID.MatchString(ref) && strings.ContainsRune("CGD", rune(ref[0])) {
		return refChannelID, ref
	}
	return refChannelName, strings.TrimPrefix(ref, "#")
}

func isUserID(s string) bool {
	return slackID.MatchString(s) && (s[0] == 'U' || s[0] == 'W')
}

// ResolveTarget turns a person into the bot's DM with them, because a mention is
// the only form in which an agent ever receives a person.
func ResolveTarget(ctx context.Context, api SlackAPI, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("Error: --to is required")
	}
	switch kind, _ := parseRef(ref); kind {
	case refUserID, refUserHandle:
		userID, err := ResolveUser(ctx, api, ref)
		if err != nil {
			return "", err
		}
		return api.OpenDM(ctx, userID)
	default:
		return ResolveChannel(ctx, api, ref)
	}
}

// ResolveChannel passes IDs through unlooked-up, because DMs and unjoined private
// channels never appear in conversations.list.
func ResolveChannel(ctx context.Context, api SlackAPI, ref string) (string, error) {
	kind, value := parseRef(ref)
	switch kind {
	case refChannelID:
		return value, nil
	case refUserID, refUserHandle:
		return "", fmt.Errorf("Error: %q names a person, not a channel. Pass #channel-name or a channel ID (C…/G…/D…).", strings.TrimSpace(ref))
	}
	channels, err := api.ListChannels(ctx)
	if err != nil {
		return "", err
	}
	for _, ch := range channels {
		if ch.Name == value || ch.ID == value {
			return ch.ID, nil
		}
	}
	return "", fmt.Errorf("Channel '%s' not found among the channels the bot can see — a private channel only appears once the bot is invited. Accepted forms: %s", value, convref.Conversation)
}

// ResolveUser prefers username over display name over real name, because only the
// username is unique within a workspace.
func ResolveUser(ctx context.Context, api SlackAPI, ref string) (string, error) {
	kind, value := parseRef(ref)
	if kind == refUserID {
		return value, nil
	}
	handle := strings.ToLower(value)
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
	return "", fmt.Errorf("User '%s' not found. Accepted forms: %s", handle, convref.User)
}

var mentionPattern = regexp.MustCompile(`@([a-zA-Z0-9._-]+)`)

// ResolveMentions leaves an unknown handle as plain text rather than failing, because
// a message with one wrong mention is still worth sending.
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
		if start > 0 && (isWordByte(text[start-1]) || text[start-1] == '<') {
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
