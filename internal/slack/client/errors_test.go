package client

import (
	"errors"
	"strings"
	"testing"
)

func TestSlackError_HintsActionableCodes(t *testing.T) {
	cases := map[string]string{
		"not_in_channel":    "/invite",
		"channel_not_found": "private channel",
		"missing_scope":     "retrying will not help",
		"invalid_blocks":    "block-kit-builder",
	}
	for code, want := range cases {
		err := slackError("conversations.history", errors.New(code))
		if !strings.HasPrefix(err.Error(), "Slack error (conversations.history): "+code) {
			t.Errorf("%s: lost the method/code prefix callers grep for: %q", code, err)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: hint missing %q: %q", code, want, err)
		}
	}
}

func TestSlackError_LeavesUnknownCodesBare(t *testing.T) {
	err := slackError("chat.postMessage", errors.New("ratelimited"))
	if err.Error() != "Slack error (chat.postMessage): ratelimited" {
		t.Fatalf("unexpected decoration on an unhinted code: %q", err)
	}
	if slackError("x", nil) != nil {
		t.Fatal("slackError(nil) must stay nil")
	}
}
