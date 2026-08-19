package cfg

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/tools"
)

// agentSchema declares every flag `cfg agent create/update` accepts. Shared
// flags apply to whichever backend --type selects: --command/--arg/--env for
// acp and claude_code, --model for native and claude_code. Booleans and arrays
// follow the CLI convention (--flag true; repeat --arg for lists).
func agentSchema(nameRequired bool) *jsonschema.Schema {
	props := map[string]*jsonschema.Schema{
		"name":                   {Type: "string", Description: "agent name (the key it is stored under)"},
		"type":                   {Type: "string", Description: "backend: native | acp | claude_code"},
		"workdir":                {Type: "string", Description: "agent working directory"},
		"tools":                  {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "tool group to expose (repeatable)"},
		"mcp_servers":            {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "extra MCP server to attach (repeatable)"},
		"export_skills_to_fs":    {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "bundled skill to export to the workdir (repeatable; 'all' for every one)"},
		"progress_display":       {Type: "string", Description: "simplified | tasks"},
		"approval_terminal":      {Type: "string", Description: "native terminal gate: allowlist | prompt | off"},
		"approval_requests":      {Type: "string", Description: "acp permission answering: ask | auto-allow | auto-deny"},
		"approval_allow":         {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "extra allowlisted terminal command (repeatable)"},
		"approval_keep_resolved": {Type: "boolean", Description: "keep settled approval cards in the thread instead of clearing them"},
		// sandbox (acp/claude_code; macOS only)
		"sandbox_mode":      {Type: "string", Description: "process confinement: off | seatbelt (macOS only)"},
		"sandbox_write":     {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "extra writable path beyond workdir/$TMPDIR/~/.claude (repeatable)"},
		"sandbox_deny_read": {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "path to blind the agent to (repeatable; omitted uses the credential-store defaults)"},
		"sandbox_env":       {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "extra env var to inherit, added to PATH/HOME/TMPDIR/USER/LANG/SHELL (repeatable)"},
		// native
		"provider":           {Type: "string", Description: "native provider: gemini | anthropic | openai"},
		"model":              {Type: "string", Description: "model id (native/claude_code)"},
		"base_url":           {Type: "string", Description: "native provider endpoint override"},
		"api_key_env":        {Type: "string", Description: "native: .env var holding the provider credential"},
		"system_prompt":      {Type: "string", Description: "native inline system prompt"},
		"system_prompt_file": {Type: "string", Description: "native system prompt file path"},
		"max_turns":          {Type: "integer", Description: "native max tool-call iterations"},
		"context_limit":      {Type: "integer", Description: "native conversation token budget"},
		"compaction":         {Type: "string", Description: "native: truncate | summarize"},
		"cache_retention":    {Type: "string", Description: "native: 5m | 1h | off"},
		// acp / claude_code
		"command": {Type: "string", Description: "backend process command (acp/claude_code)"},
		"arg":     {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "backend process argument (repeatable; acp/claude_code)"},
		"env":     {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "backend env var KEY=VALUE (repeatable; acp/claude_code)"},
	}
	_ = nameRequired // required-ness is enforced in Invoke, not the schema
	return &jsonschema.Schema{Type: "object", Properties: props}
}

// agentCreateTool creates a new agent from typed flags.
type agentCreateTool struct{ p Provider }

func (t *agentCreateTool) Name() string { return "cfg.agent.create" }
func (t *agentCreateTool) Description() string {
	return "Create an agent (e.g. `cfg agent create --name code --type claude-code --command claude`)."
}
func (t *agentCreateTool) InputSchema() *jsonschema.Schema { return agentSchema(true) }
func (t *agentCreateTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	name, err := requireString(args, "name")
	if err != nil {
		return nil, err
	}
	if _, ok := stringArg(args, "type"); !ok {
		return nil, fmt.Errorf("--type is required (native | acp | claude_code)")
	}
	s, err := t.p()
	if err != nil {
		return nil, err
	}
	if _, exists, err := s.GetItem(ctx, config.SectionAgent, name); err != nil {
		return nil, err
	} else if exists {
		return nil, fmt.Errorf("agent %q already exists (use `cfg agent update`)", name)
	}
	profile, err := buildAgentProfile(nil, args)
	if err != nil {
		return nil, err
	}
	if err := upsertItemValidated(ctx, s, config.SectionAgent, name, profile); err != nil {
		return nil, err
	}
	return okResult{Message: fmt.Sprintf("created agent %q (%s)", name, profile.ResolvedKind())}, nil
}

// agentUpdateTool updates an existing agent, applying only the flags given.
type agentUpdateTool struct{ p Provider }

func (t *agentUpdateTool) Name() string { return "cfg.agent.update" }
func (t *agentUpdateTool) Description() string {
	return "Update an existing agent; only the flags you pass are changed."
}
func (t *agentUpdateTool) InputSchema() *jsonschema.Schema { return agentSchema(false) }
func (t *agentUpdateTool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	name, err := requireString(args, "name")
	if err != nil {
		return nil, err
	}
	s, err := t.p()
	if err != nil {
		return nil, err
	}
	body, ok, err := s.GetItem(ctx, config.SectionAgent, name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("agent %q not found (use `cfg agent create`)", name)
	}
	var existing config.AgentProfile
	if err := json.Unmarshal(body, &existing); err != nil {
		return nil, err
	}
	profile, err := buildAgentProfile(&existing, args)
	if err != nil {
		return nil, err
	}
	if err := upsertItemValidated(ctx, s, config.SectionAgent, name, profile); err != nil {
		return nil, err
	}
	return okResult{Message: fmt.Sprintf("updated agent %q (%s)", name, profile.ResolvedKind())}, nil
}

// buildAgentProfile applies the provided flags onto a base profile (nil for
// create, the existing profile for update). Passing --type (re)selects the
// backend sub-block; backend-specific flags then apply to the active block.
func buildAgentProfile(existing *config.AgentProfile, args map[string]any) (config.AgentProfile, error) {
	var p config.AgentProfile
	if existing != nil {
		p = *existing
	}

	if v, ok := stringArg(args, "workdir"); ok {
		p.WorkDir = v
	}
	if v, ok := arrayArg(args, "tools"); ok {
		p.Tools = v
	}
	if v, ok := arrayArg(args, "mcp_servers"); ok {
		p.MCPServers = v
	}
	if v, ok := arrayArg(args, "export_skills_to_fs"); ok {
		p.ExportSkillsToFS = v
	}
	if v, ok := stringArg(args, "progress_display"); ok {
		p.ProgressDisplay = v
	}
	if v, ok := stringArg(args, "approval_terminal"); ok {
		p.Approval.Terminal = v
	}
	if v, ok := stringArg(args, "approval_requests"); ok {
		p.Approval.Requests = v
	}
	if v, ok := arrayArg(args, "approval_allow"); ok {
		p.Approval.Allow = v
	}
	if v, ok := boolArg(args, "approval_keep_resolved"); ok {
		p.Approval.KeepResolved = &v
	}

	if v, ok := stringArg(args, "sandbox_mode"); ok {
		p.Sandbox.Mode = v
	}
	if v, ok := arrayArg(args, "sandbox_write"); ok {
		p.Sandbox.Write = v
	}
	if v, ok := arrayArg(args, "sandbox_deny_read"); ok {
		p.Sandbox.DenyRead = v
	}
	if v, ok := arrayArg(args, "sandbox_env"); ok {
		p.Sandbox.Env = v
	}

	if typ, ok := stringArg(args, "type"); ok {
		switch normalizeType(typ) {
		case config.AgentKindNative:
			if p.Native == nil {
				p.Native = &config.NativeProfile{}
			}
			p.ACP, p.ClaudeCode = nil, nil
		case config.AgentKindACP:
			if p.ACP == nil {
				p.ACP = &config.ACPProfile{}
			}
			p.Native, p.ClaudeCode = nil, nil
		case config.AgentKindClaudeCode:
			if p.ClaudeCode == nil {
				p.ClaudeCode = &config.ClaudeCodeProfile{}
			}
			p.Native, p.ACP = nil, nil
		default:
			return config.AgentProfile{}, fmt.Errorf("--type must be native, acp, or claude_code (got %q)", typ)
		}
	}

	if p.Native != nil {
		applyNative(p.Native, args)
	}
	if p.ACP != nil {
		if err := applyACP(p.ACP, args); err != nil {
			return config.AgentProfile{}, err
		}
	}
	if p.ClaudeCode != nil {
		if err := applyClaudeCode(p.ClaudeCode, args); err != nil {
			return config.AgentProfile{}, err
		}
	}
	return p, nil
}

func normalizeType(t string) config.AgentKind {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "native":
		return config.AgentKindNative
	case "acp":
		return config.AgentKindACP
	case "claude_code", "claude-code", "claudecode":
		return config.AgentKindClaudeCode
	default:
		return ""
	}
}

func applyNative(n *config.NativeProfile, args map[string]any) {
	if v, ok := stringArg(args, "provider"); ok {
		n.Provider = v
	}
	if v, ok := stringArg(args, "model"); ok {
		n.Model = v
	}
	if v, ok := stringArg(args, "base_url"); ok {
		n.BaseURL = v
	}
	if v, ok := stringArg(args, "api_key_env"); ok {
		n.APIKeyEnv = v
	}
	if v, ok := stringArg(args, "system_prompt"); ok {
		n.SystemPrompt = v
	}
	if v, ok := stringArg(args, "system_prompt_file"); ok {
		n.SystemPromptFile = v
	}
	if v, ok := intArg(args, "max_turns"); ok {
		n.MaxTurns = v
	}
	if v, ok := intArg(args, "context_limit"); ok {
		n.ContextLimit = v
	}
	if v, ok := stringArg(args, "compaction"); ok {
		n.Compaction = v
	}
	if v, ok := stringArg(args, "cache_retention"); ok {
		n.CacheRetention = v
	}
}

func applyACP(a *config.ACPProfile, args map[string]any) error {
	if v, ok := stringArg(args, "command"); ok {
		a.Command = v
	}
	if v, ok := arrayArg(args, "arg"); ok {
		a.Args = v
	}
	if env, ok, err := envArg(args, "env"); err != nil {
		return err
	} else if ok {
		a.Env = env
	}
	return nil
}

func applyClaudeCode(c *config.ClaudeCodeProfile, args map[string]any) error {
	if v, ok := stringArg(args, "command"); ok {
		c.Command = v
	}
	if v, ok := arrayArg(args, "arg"); ok {
		c.Args = v
	}
	if v, ok := stringArg(args, "model"); ok {
		c.Model = v
	}
	if env, ok, err := envArg(args, "env"); err != nil {
		return err
	} else if ok {
		c.Env = env
	}
	return nil
}

// AgentTools returns the agent create/update tools plus the shared
// list/show/delete trio.
func AgentTools(p Provider) []tools.Tool {
	out := []tools.Tool{&agentCreateTool{p: p}, &agentUpdateTool{p: p}}
	return append(out, sectionTools(p, config.SectionAgent, "cfg.agent", "agent")...)
}
