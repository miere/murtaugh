package client

import (
	"fmt"
	"strconv"
	"time"
)

// SydneyTZ is the canonical timezone for --since interpretation.
const SydneyTZ = "Australia/Sydney"

// SinceFormat is the textual format --since values use: 'YYYY-MM-DD HH:mm:ss'.
const SinceFormat = "2006-01-02 15:04:05"

// SinceParseError is the user-facing string emitted when --since fails to
// parse.
const SinceParseError = "Error: --since must be in format 'YYYY-MM-DD HH:mm:ss' (Sydney time)."

// ParseSince parses raw as Sydney-local time. An empty string defaults to
// `now - 24h` (Sydney). Invalid input returns an error whose message matches
// SinceParseError.
func ParseSince(raw string) (time.Time, error) {
	loc, err := time.LoadLocation(SydneyTZ)
	if err != nil {
		return time.Time{}, fmt.Errorf("load timezone %s: %w", SydneyTZ, err)
	}
	if raw == "" {
		return time.Now().In(loc).Add(-24 * time.Hour), nil
	}
	t, err := time.ParseInLocation(SinceFormat, raw, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s", SinceParseError)
	}
	return t, nil
}

// SlackTS converts a Go time to the string form Slack's `oldest=` parameter
// expects: a unix timestamp with microsecond precision.
func SlackTS(t time.Time) string {
	return strconv.FormatFloat(float64(t.UnixNano())/1e9, 'f', 6, 64)
}
