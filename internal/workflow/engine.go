package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"text/template"

	"github.com/miere/murtaugh-dev-toolkit/assets"
	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/slack-go/slack"
)

type Engine struct {
	rules       []Rule
	poster      ResponsePoster
	runner      CommandRunner
	templateDir string
	templateFS  fs.FS
	logger      *slog.Logger
}

type Rule struct {
	Name   string
	Config config.WorkflowRuleConfig
}

type Options struct {
	Poster      ResponsePoster
	Runner      CommandRunner
	TemplateDir string
	TemplateFS  fs.FS
	Logger      *slog.Logger
}

func NewEngine(cfg config.Config, opts Options) *Engine {
	rulesConfig := cfg.WorkflowRules
	templateFS := opts.TemplateFS
	if templateFS == nil {
		templateFS = assets.FS
	}
	if len(rulesConfig) == 0 {
		rulesConfig = defaultWorkflowRules()
	}

	names := make([]string, 0, len(rulesConfig))
	for name := range rulesConfig {
		names = append(names, name)
	}
	sort.Strings(names)

	rules := make([]Rule, 0, len(names))
	for _, name := range names {
		rules = append(rules, Rule{Name: name, Config: rulesConfig[name]})
	}

	templateDir := opts.TemplateDir
	if templateDir == "" {
		templateDir = cfg.BaseDir
	}
	if templateDir == "" {
		templateDir = "."
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	poster := opts.Poster
	if poster == nil {
		poster = HTTPResponsePoster{}
	}
	runner := opts.Runner
	if runner == nil {
		runner = OSCommandRunner{}
	}

	return &Engine{rules: rules, poster: poster, runner: runner, templateDir: templateDir, templateFS: templateFS, logger: logger}
}

func defaultWorkflowRules() map[string]config.WorkflowRuleConfig {
	return map[string]config.WorkflowRuleConfig{
		"ping-pong": {
			RequestEvent: "interactive",
			Match: map[string]any{
				"type":    "block_actions",
				"actions": []any{map[string]any{"action_id": "ping"}},
			},
			Triggers: []config.TriggerConfig{{
				Type:         "reply-to-slack",
				ReplyToSlack: &config.ReplyToSlackTriggerConfig{Template: "ping/02-pong.json"},
			}},
		},
	}
}

func (e *Engine) Execute(ctx context.Context, interaction slack.InteractionCallback) error {
	payload, err := payloadMap(interaction)
	if err != nil {
		return err
	}
	payloadJSON, err := json.Marshal(interaction)
	if err != nil {
		return fmt.Errorf("marshal interaction payload: %w", err)
	}

	for _, rule := range e.rules {
		if rule.Config.RequestEvent != "interactive" || !matches(rule.Config.Match, payload) {
			continue
		}
		e.logger.Info("workflow rule matched", "rule", rule.Name)
		return e.executeRule(ctx, rule, interaction.ResponseURL, payload, payloadJSON)
	}

	e.logger.Info(
		"interactive request had no matching workflow rule",
		"interaction_type", interaction.Type,
		"channel", interaction.Channel.Name,
		"callback_id", interaction.CallbackID,
		"action_ids", blockActionIDs(interaction.ActionCallback.BlockActions),
	)
	return nil
}

func (e *Engine) executeRule(ctx context.Context, rule Rule, responseURL string, payload map[string]any, payloadJSON []byte) error {
	for _, trigger := range rule.Config.Triggers {
		switch trigger.Type {
		case "reply-to-slack":
			body, err := e.renderReply(ctx, *trigger.ReplyToSlack, payload, payloadJSON)
			if err != nil {
				return fmt.Errorf("render Slack response for rule %s: %w", rule.Name, err)
			}
			if err := e.poster.Post(ctx, responseURL, body); err != nil {
				return fmt.Errorf("post Slack response for rule %s: %w", rule.Name, err)
			}
		case "run":
			if _, err := e.runner.Run(ctx, *trigger.Run, payloadJSON); err != nil {
				return fmt.Errorf("run command for rule %s: %w", rule.Name, err)
			}
		}
	}
	return nil
}

func (e *Engine) renderReply(ctx context.Context, trigger config.ReplyToSlackTriggerConfig, payload map[string]any, payloadJSON []byte) ([]byte, error) {
	if trigger.Run != nil {
		stdout, err := e.runner.Run(ctx, *trigger.Run, payloadJSON)
		if err != nil {
			return nil, err
		}
		return validJSON(stdout)
	}

	path := e.templatePath(trigger.Template)
	content, err := e.readTemplate(trigger.Template, path)
	if err != nil {
		return nil, fmt.Errorf("read template: %w", err)
	}
	tpl, err := template.New(filepath.Base(path)).Option("missingkey=error").Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}

	var rendered bytes.Buffer
	data := map[string]any{"Payload": payload}
	if err := tpl.Execute(&rendered, data); err != nil {
		return nil, fmt.Errorf("execute template: %w", err)
	}
	return validJSON(rendered.Bytes())
}

func (e *Engine) templatePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(e.templateDir, path)
}

func (e *Engine) readTemplate(templatePath string, resolvedPath string) ([]byte, error) {
	content, err := os.ReadFile(resolvedPath)
	if err == nil {
		return content, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if e.templateFS != nil && !filepath.IsAbs(templatePath) {
		return fs.ReadFile(e.templateFS, filepath.ToSlash(templatePath))
	}
	return nil, err
}

func blockActionIDs(actions []*slack.BlockAction) []string {
	ids := make([]string, 0, len(actions))
	for _, action := range actions {
		if action == nil {
			continue
		}
		ids = append(ids, action.ActionID)
	}
	return ids
}

func validJSON(body []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(body)
	if !json.Valid(trimmed) {
		return nil, fmt.Errorf("rendered Slack response must be valid JSON")
	}
	return trimmed, nil
}
