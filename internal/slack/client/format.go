package client

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Reaction is the public projection of a Slack message reaction the tools
// expose to the MCP frontend. It is decoupled from slack-go's own type so
// tests can construct it without pulling the SDK in.
type Reaction struct {
	Name  string   `json:"name"`
	Users []string `json:"users"`
	Count int      `json:"count"`
}

// Message is the public projection of a Slack message used by all four
// Slack tools.
type Message struct {
	TS        string     `json:"ts"`
	User      string     `json:"user"`
	Text      string     `json:"text"`
	ThreadTS  string     `json:"thread_ts,omitempty"`
	Reactions []Reaction `json:"reactions,omitempty"`
}

// FormatMessage renders a single message as `[HH:MM] @user: text` in Sydney
// time. An empty user field falls back to "unknown".
func FormatMessage(m Message) string {
	user := m.User
	if user == "" {
		user = "unknown"
	}
	loc, err := time.LoadLocation(SydneyTZ)
	if err != nil {
		return fmt.Sprintf("[??:??] @%s: %s", user, m.Text)
	}
	hhmm := "??:??"
	if ts, err := strconv.ParseFloat(m.TS, 64); err == nil {
		sec, nsec := int64(ts), int64((ts-float64(int64(ts)))*1e9)
		hhmm = time.Unix(sec, nsec).In(loc).Format("15:04")
	}
	return fmt.Sprintf("[%s] @%s: %s", hhmm, user, m.Text)
}

// FormatMessages joins the per-message lines for CLI output. The input is
// expected oldest-first; callers that have a newest-first slice should
// reverse it before invoking this helper.
func FormatMessages(msgs []Message) string {
	if len(msgs) == 0 {
		return ""
	}
	lines := make([]string, 0, len(msgs))
	for _, m := range msgs {
		lines = append(lines, FormatMessage(m))
	}
	return strings.Join(lines, "\n")
}

// ReverseMessages returns a copy of msgs with the order inverted. Slack's
// conversations.history returns newest-first; the tools want oldest-first.
func ReverseMessages(msgs []Message) []Message {
	out := make([]Message, len(msgs))
	for i, m := range msgs {
		out[len(msgs)-1-i] = m
	}
	return out
}
