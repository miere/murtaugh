package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const baseSlackYAML = `oauth:
  app_token: xapp-test
  bot_token: xoxb-test
`

func testConfig(extra string) []byte {
	return []byte(baseSlackYAML + extra)
}

func TestParseValidConfig(t *testing.T) {
	cfg, err := Parse(testConfig(`configuration:
  admin_user: '@admin'
chat:
  default_agent: default
  channel_agents:
    C12345: coding
  dm_agent: default
commands:
  - name: /murtaugh
`))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	if cfg.OAuth.AppToken != "xapp-test" || cfg.OAuth.BotToken != "xoxb-test" || cfg.Configuration.AdminUser != "@admin" {
		t.Fatalf("unexpected Slack config parsed")
	}
	if cfg.Chat.DefaultAgent != "default" || cfg.Chat.DMAgent != "default" || cfg.Chat.ChannelAgents["C12345"] != "coding" {
		t.Fatalf("unexpected chat routing parsed: %#v", cfg.Chat)
	}
	if len(cfg.Commands) != 1 || cfg.Commands[0].Name != "/murtaugh" {
		t.Fatalf("unexpected commands parsed: %#v", cfg.Commands)
	}
}

func TestLoadACPConfigFromAgentsFile(t *testing.T) {
	baseDir := t.TempDir()
	configPath := filepath.Join(baseDir, "slack.yaml")
	if err := os.WriteFile(configPath, testConfig(`chat:
  default_agent: default
`), 0o644); err != nil {
		t.Fatalf("write slack config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(baseDir, "agents.yaml"), []byte(`acp:
  enabled: true
  request_timeout: 2m
  stream_append_interval: 100ms
  stream_min_chunk_chars: 12
agents:
  default:
    command: ls
`), 0o644); err != nil {
		t.Fatalf("write agents config: %v", err)
	}

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if !cfg.ACP.Enabled || cfg.ACP.EffectiveStreamMinChunkChars() != 12 {
		t.Fatalf("unexpected ACP config: %#v", cfg.ACP)
	}
	if _, ok := cfg.Agents["default"]; !ok {
		t.Fatalf("expected default agent to be loaded: %#v", cfg.Agents)
	}
}

func TestParseACPRequiresAgentsWhenEnabled(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.ACP.Enabled = true
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "no agents are defined") {
		t.Fatalf("expected ACP agents validation error, got: %v", err)
	}
}

func TestParseACPValidatesDurations(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.ACP.RequestTimeout = "nope"
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "acp.request_timeout") {
		t.Fatalf("expected ACP duration validation error, got: %v", err)
	}
}

func TestParseRequiresSlackTokens(t *testing.T) {
	cfg, err := Parse([]byte("oauth: {}\n"))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	message := err.Error()
	if !strings.Contains(message, "oauth.app_token") || !strings.Contains(message, "oauth.bot_token") {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestParseValidatesSlashCommandNames(t *testing.T) {
	cfg, err := Parse(testConfig("commands:\n  - name: murtaugh\n"))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "must start with /") {
		t.Fatalf("expected slash command validation error, got: %v", err)
	}
}

func TestParseWorkflowRules(t *testing.T) {
	cfg, err := Parse(testConfig(`workflow-rules:
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
	cfg, err := Parse(testConfig(`workflow-rules:
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
	cfg, err := Parse(testConfig(`workflow-rules:
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
	cfg, err := Parse(testConfig(`unfurl-rules:
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
	cfg, err := Parse(testConfig(`unfurl-rules:
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
	cfg, err := Parse(testConfig(`unfurl-rules:
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

func TestParseJobsConfig(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	// Inject jobs manually (as Load() would, from jobs.yaml)
	cfg.Jobs = map[string]JobProfile{
		"cleanup-logs": {
			Command: "/usr/bin/find",
			Args:    []string{"/var/log", "-mtime", "+7", "-delete"},
			WorkDir: "/tmp",
			Timeout: "5m",
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
	job, ok := cfg.Jobs["cleanup-logs"]
	if !ok {
		t.Fatal("expected cleanup-logs job")
	}
	if job.Command != "/usr/bin/find" || len(job.Args) != 4 || job.Timeout != "5m" {
		t.Fatalf("unexpected job parsed: %#v", job)
	}
}

func TestJobValidationRequiresCommand(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.Jobs = map[string]JobProfile{
		"bad-job": {Command: ""},
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "jobs[bad-job].command is required") {
		t.Fatalf("expected job command validation error, got: %v", err)
	}
}

func TestJobValidationRejectsBadTimeout(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.Jobs = map[string]JobProfile{
		"bad-timeout": {Command: "/bin/echo", Timeout: "nope"},
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "jobs[bad-timeout].timeout") {
		t.Fatalf("expected job timeout validation error, got: %v", err)
	}
}

func TestJobValidationAcceptsOptionalFields(t *testing.T) {
	cfg, err := Parse(testConfig(""))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	cfg.Jobs = map[string]JobProfile{
		"minimal": {Command: "/bin/echo"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}

func TestParseUnfurlRejectsBadRegex(t *testing.T) {
	cfg, err := Parse(testConfig(`unfurl-rules:
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
