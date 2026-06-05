package config

import (
	"strings"
	"testing"
)

func TestParseValidConfig(t *testing.T) {
	cfg, err := Parse([]byte("slack:\n  app_token: xapp-test\n  bot_token: xoxb-test\n  admin_user: '@admin'\ncommands:\n  - name: /murtaugh\n"))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if cfg.Slack.AppToken != "xapp-test" || cfg.Slack.BotToken != "xoxb-test" || cfg.Slack.AdminUser != "@admin" {
		t.Fatalf("unexpected Slack tokens parsed")
	}
	if len(cfg.Commands) != 1 || cfg.Commands[0].Name != "/murtaugh" {
		t.Fatalf("unexpected commands parsed: %#v", cfg.Commands)
	}
}

func TestParseACPConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
slack:
  app_token: xapp-test
  bot_token: xoxb-test
  default_agent: default
acp:
  enabled: true
  request_timeout: 2m
  stream_append_interval: 100ms
  stream_min_chunk_chars: 12
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	// Manually add agent for validation success
	cfg.Agents = map[string]AgentProfile{"default": {Command: "ls"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if !cfg.ACP.Enabled || cfg.ACP.EffectiveStreamMinChunkChars() != 12 {
		t.Fatalf("unexpected ACP config: %#v", cfg.ACP)
	}
}

func TestParseACPRequiresAgentsWhenEnabled(t *testing.T) {
	cfg, err := Parse([]byte("slack:\n  app_token: xapp-test\n  bot_token: xoxb-test\nacp:\n  enabled: true\n"))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "no agents are defined") {
		t.Fatalf("expected ACP agents validation error, got: %v", err)
	}
}

func TestParseACPValidatesDurations(t *testing.T) {
	cfg, err := Parse([]byte("slack:\n  app_token: xapp-test\n  bot_token: xoxb-test\nacp:\n  request_timeout: nope\n"))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "acp.request_timeout") {
		t.Fatalf("expected ACP duration validation error, got: %v", err)
	}
}

func TestParseRequiresSlackTokens(t *testing.T) {
	cfg, err := Parse([]byte("slack: {}\n"))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	message := err.Error()
	if !strings.Contains(message, "slack.app_token") || !strings.Contains(message, "slack.bot_token") {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestParseValidatesSlashCommandNames(t *testing.T) {
	cfg, err := Parse([]byte("slack:\n  app_token: xapp-test\n  bot_token: xoxb-test\ncommands:\n  - name: murtaugh\n"))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "must start with /") {
		t.Fatalf("expected slash command validation error, got: %v", err)
	}
}

func TestParseWorkflowRules(t *testing.T) {
	cfg, err := Parse([]byte(`
slack:
  app_token: xapp-test
  bot_token: xoxb-test
workflow-rules:
  code-review-approval:
    request_event: interactive
    match:
      channel: { name: nc-code-reviews }
      actions:
        - block_id: github_pull_request
          action_id: approve_only
    trigger:
      - reply-to-slack:
          template: code-review/02-approved.json
      - run:
          cmd: /path/to/cmd
          args: [param1, param2]
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	rule := cfg.WorkflowRules["code-review-approval"]
	if rule.RequestEvent != "interactive" || len(rule.Triggers) != 2 {
		t.Fatalf("unexpected workflow rule parsed: %#v", rule)
	}
	if rule.Triggers[0].ReplyToSlack.Template != "code-review/02-approved.json" {
		t.Fatalf("unexpected reply-to-slack trigger: %#v", rule.Triggers[0])
	}
	if rule.Triggers[1].Run.Cmd != "/path/to/cmd" || len(rule.Triggers[1].Run.Args) != 2 {
		t.Fatalf("unexpected run trigger: %#v", rule.Triggers[1])
	}
}

func TestParseWorkflowRuleValidatesReplyToSlackRenderer(t *testing.T) {
	cfg, err := Parse([]byte(`
slack:
  app_token: xapp-test
  bot_token: xoxb-test
workflow-rules:
  invalid:
    request_event: interactive
    match:
      type: block_actions
    trigger:
      - reply-to-slack:
          template: response.json
          run:
            cmd: /path/to/cmd
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "exactly one of template or run") {
		t.Fatalf("expected reply-to-slack validation error, got: %v", err)
	}
}

func TestParseWorkflowRuleValidatesRequestEvent(t *testing.T) {
	cfg, err := Parse([]byte(`
slack:
  app_token: xapp-test
  bot_token: xoxb-test
workflow-rules:
  invalid:
    request_event: slash_command
    match:
      type: block_actions
    trigger:
      - run:
          cmd: /path/to/cmd
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "request_event must be interactive") {
		t.Fatalf("expected request event validation error, got: %v", err)
	}
}

func TestParseValidUnfurlRule(t *testing.T) {
	cfg, err := Parse([]byte(`
slack:
  app_token: xapp-test
  bot_token: xoxb-test
unfurl-rules:
  github-pr:
    match:
      channels: [C0ENG]
      domain: github.com
      url_pattern: '/pull/(?P<number>\d+)'
    unfurl:
      template: unfurl/github-pr.json
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	rule, ok := cfg.UnfurlRules["github-pr"]
	if !ok {
		t.Fatal("expected github-pr unfurl rule")
	}
	if rule.Match.Domain != "github.com" || rule.Unfurl.Template != "unfurl/github-pr.json" {
		t.Fatalf("unexpected unfurl rule parsed: %#v", rule)
	}
	if len(rule.Match.Channels) != 1 || rule.Match.Channels[0] != "C0ENG" {
		t.Fatalf("unexpected channels parsed: %#v", rule.Match.Channels)
	}
}

func TestParseUnfurlRequiresMatchCondition(t *testing.T) {
	cfg, err := Parse([]byte(`
slack:
  app_token: xapp-test
  bot_token: xoxb-test
unfurl-rules:
  bad:
    match:
      channels: [C1]
    unfurl:
      template: t.json
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "at least one of domain") {
		t.Fatalf("expected match condition error, got: %v", err)
	}
}

func TestParseUnfurlRejectsTemplateAndRun(t *testing.T) {
	cfg, err := Parse([]byte(`
slack:
  app_token: xapp-test
  bot_token: xoxb-test
unfurl-rules:
  bad:
    match:
      domain: github.com
    unfurl:
      template: t.json
      run:
        cmd: echo
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "exactly one of template or run") {
		t.Fatalf("expected exclusivity error, got: %v", err)
	}
}

func TestParseUnfurlRejectsBadRegex(t *testing.T) {
	cfg, err := Parse([]byte(`
slack:
  app_token: xapp-test
  bot_token: xoxb-test
unfurl-rules:
  bad:
    match:
      domain: github.com
      url_pattern: '([a-z'
    unfurl:
      template: t.json
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "url_pattern") {
		t.Fatalf("expected url_pattern validation error, got: %v", err)
	}
}
