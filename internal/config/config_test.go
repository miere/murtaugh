package config

import (
	"strings"
	"testing"
)

func TestParseValidConfig(t *testing.T) {
	cfg, err := Parse([]byte("slack:\n  app_token: xapp-test\n  bot_token: xoxb-test\ncommands:\n  - name: /murtaugh\n"))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if cfg.Slack.AppToken != "xapp-test" || cfg.Slack.BotToken != "xoxb-test" {
		t.Fatalf("unexpected Slack tokens parsed")
	}
	if len(cfg.Commands) != 1 || cfg.Commands[0].Name != "/murtaugh" {
		t.Fatalf("unexpected commands parsed: %#v", cfg.Commands)
	}
}

func TestParseRequiresSlackTokens(t *testing.T) {
	_, err := Parse([]byte("slack: {}\n"))
	if err == nil {
		t.Fatal("expected validation error")
	}
	message := err.Error()
	if !strings.Contains(message, "slack.app_token") || !strings.Contains(message, "slack.bot_token") {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestParseValidatesSlashCommandNames(t *testing.T) {
	_, err := Parse([]byte("slack:\n  app_token: xapp-test\n  bot_token: xoxb-test\ncommands:\n  - name: murtaugh\n"))
	if err == nil || !strings.Contains(err.Error(), "must start with /") {
		t.Fatalf("expected slash command validation error, got: %v", err)
	}
}
