// Package slack implements the `setup.slack` tool: write the Slack OAuth block
// to the bootstrap gateway.yaml (preserving its database block) and record the
// admin user + chat routing in the configuration store.
//
// The tool is deliberately narrow: it owns Slack credentials + the access/chat
// entry point. Agent configuration is owned by `setup.agents` / `cfg agent`,
// MCP wiring by `setup.mcp-register`.
package slack

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/store"
	"github.com/miere/murtaugh/internal/tools/setup/internal/envfile"
)

// Env variable names gateway.yaml references for the Slack credentials. The actual
// tokens live in ~/.config/murtaugh/.env, never in the YAML — so a shared config
// file (or a troubleshoot bundle) carries only the ${VAR} references.
const (
	appTokenVar = "SLACK_APP_TOKEN"
	botTokenVar = "SLACK_BOT_TOKEN"
)

// PathProvider returns the absolute path of gateway.yaml. A closure over the
// loaded config dir is supplied by the composition root so the same path is
// observed whether the tool runs via the CLI, MCP, or a direct test.
type PathProvider func() string

// StoreProvider yields the open configuration store (access + chat live there).
type StoreProvider func() (config.Store, error)

// Tool is the `setup.slack` capability.
type Tool struct {
	path  PathProvider
	store StoreProvider
}

// New constructs a Tool that writes gateway.yaml at the path returned by path
// and the access/chat singletons into the store returned by provider.
func New(path PathProvider, provider StoreProvider) *Tool {
	return &Tool{path: path, store: provider}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "setup.slack" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Write Slack OAuth to gateway.yaml and the admin user + chat routing to the config store."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"app_token":     {Type: "string", Description: "Slack app-level token (must start with xapp-)."},
			"bot_token":     {Type: "string", Description: "Slack bot OAuth token (must start with xoxb-)."},
			"admin_user":    {Type: "string", Description: "Slack admin handle (@name) or user ID (U…)."},
			"default_agent": {Type: "string", Description: "Optional agent name wired into chat.defaults.agent (also enables chat)."},
		},
		Required: []string{"app_token", "bot_token", "admin_user"},
	}
}

// Result is the structured payload returned by Invoke.
type Result struct {
	Path string `json:"path"`
	// EnvPath is the .env the Slack tokens were written to (referenced from
	// gateway.yaml as ${SLACK_APP_TOKEN}/${SLACK_BOT_TOKEN}).
	EnvPath     string `json:"env_path,omitempty"`
	ChatEnabled bool   `json:"chat_enabled"`
}

// String renders a one-line CLI confirmation. It never echoes the tokens.
func (r Result) String() string {
	state := "chat off"
	if r.ChatEnabled {
		state = "chat on"
	}
	return fmt.Sprintf("wrote Slack oauth → %s (tokens → %s); admin + %s in the config store", r.Path, r.EnvPath, state)
}

// Invoke validates arguments, writes the Slack tokens to .env, sets the oauth
// block in gateway.yaml (preserving the database block), and stores the access
// and chat singletons.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	appToken, _ := args["app_token"].(string)
	botToken, _ := args["bot_token"].(string)
	adminUser, _ := args["admin_user"].(string)
	defaultAgent, _ := args["default_agent"].(string)

	if !strings.HasPrefix(appToken, "xapp-") {
		return nil, errors.New("app_token must start with xapp-")
	}
	if !strings.HasPrefix(botToken, "xoxb-") {
		return nil, errors.New("bot_token must start with xoxb-")
	}
	if strings.TrimSpace(adminUser) == "" {
		return nil, errors.New("admin_user is required")
	}

	path := t.path()
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("gateway.yaml path is not configured")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("ensure config dir: %w", err)
	}

	// Secrets go to the .env sibling; gateway.yaml only references them.
	envPath := filepath.Join(filepath.Dir(path), ".env")
	if _, err := envfile.Merge(envPath, map[string]string{
		appTokenVar: appToken,
		botTokenVar: botToken,
	}); err != nil {
		return nil, fmt.Errorf("write Slack tokens to .env: %w", err)
	}

	// Set the oauth block, preserving whatever database block is already present.
	oauth := config.OAuthConfig{AppToken: "${" + appTokenVar + "}", BotToken: "${" + botTokenVar + "}"}
	if err := store.SetBootstrapOAuth(path, oauth); err != nil {
		return nil, err
	}

	// Access + chat live in the store now.
	s, err := t.store()
	if err != nil {
		return nil, err
	}
	if err := s.PutSingleton(ctx, config.SingletonAccess, config.AccessConfig{AdminUser: adminUser}); err != nil {
		return nil, fmt.Errorf("store access config: %w", err)
	}
	chat := config.ChatConfig{}
	if da := strings.TrimSpace(defaultAgent); da != "" {
		chat.Enabled = true
		chat.Defaults = config.ChatDefaults{Agent: da}
	}
	if err := s.PutSingleton(ctx, config.SingletonChat, chat); err != nil {
		return nil, fmt.Errorf("store chat config: %w", err)
	}

	return Result{Path: path, EnvPath: envPath, ChatEnabled: chat.Enabled}, nil
}
