// Package agents implements the `setup.agents` tool: register the runtime
// defaults and a single named agent in the configuration store. It supports both
// backends — a native LLM agent (a `native:` block, the default) and an external
// ACP agent (an `acp:` block) — so the installer can configure either from one
// tool. It is a thin installer convenience over the same store the richer
// `cfg agent create` / `cfg defaults set` tools write to.
//
// Secrets are never written here: a native profile only records api_key_env (the
// .env variable name); the key value goes to .env via setup.env.
package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/config"
)

// StoreProvider yields the open configuration store.
type StoreProvider func() (config.Store, error)

// Tool is the `setup.agents` capability.
type Tool struct {
	store StoreProvider
}

// New constructs a Tool that writes into the store returned by provider.
func New(provider StoreProvider) *Tool {
	return &Tool{store: provider}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "setup.agents" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Register the runtime defaults and a native (default) or ACP agent in the config store."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"agent_name": {Type: "string", Description: "Key under which the agent is registered. Defaults to \"default\"."},
			"kind":       {Type: "string", Description: "Backend: \"native\" (default) or \"acp\". Inferred from the flags when omitted."},
			// ACP backend.
			"command": {Type: "string", Description: "ACP only: absolute path to the ACP-speaking binary."},
			"args":    {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "ACP only: arguments passed to command."},
			// Native backend.
			"provider":           {Type: "string", Description: "Native: provider family — gemini, anthropic, or openai."},
			"model":              {Type: "string", Description: "Native: provider model id (e.g. gemini-2.5-pro)."},
			"base_url":           {Type: "string", Description: "Native: endpoint override for compat providers (Z.ai/DeepSeek/Kimi)."},
			"api_key_env":        {Type: "string", Description: "Native: name of the .env variable holding the API key."},
			"tools":              {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "Native: tool allowlist (files, terminal, skills, attach, and registry namespaces)."},
			"mcp_servers":        {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: "Native: names of mcp_servers entries to attach."},
			"system_prompt_file": {Type: "string", Description: "Native: path (relative to config dir) to the system prompt file."},
			"context_limit":      {Type: "integer", Description: "Native: token budget for compaction. 0 uses a per-family default."},
			"compaction":         {Type: "string", Description: "Native: \"truncate\" (default) or \"summarize\"."},
			"cache_retention":    {Type: "string", Description: "Native: prompt-cache TTL — \"5m\" (default), \"1h\", or \"off\"."},
		},
	}
}

// Result is the structured payload returned by Invoke. Enabled reports whether
// an agent was configured (not whether chat is on — that gate, chat.enabled,
// lives in the chat singleton and is written by cfg chat set).
type Result struct {
	Created   bool   `json:"created"`
	Enabled   bool   `json:"enabled"`
	AgentName string `json:"agent_name,omitempty"`
	Kind      string `json:"kind,omitempty"`
}

// String renders a one-line CLI confirmation.
func (r Result) String() string {
	verb := "updated"
	if r.Created {
		verb = "created"
	}
	if !r.Enabled {
		return "runtime defaults set (no agent configured)"
	}
	return fmt.Sprintf("%s agent %q (%s) in the config store", verb, r.AgentName, r.Kind)
}

// runtimeDefaults is the runtime tuning the installer establishes on first run,
// split by the concern each knob serves.
var runtimeDefaults = config.RuntimeDefaults{
	Session:   config.SessionDefaults{IdleTimeout: "30m", RequestTimeout: "10m", LongRunningToolTimeout: "1h", MaxConcurrent: 100},
	Rendering: config.RenderingDefaults{ProgressDisplay: "simplified", StreamMinChunkChars: 96, StreamAppendInterval: "750ms"},
	ACP:       config.ACPDefaults{StartupTimeout: "10s", CancelGracePeriod: "2s"},
}

// Invoke validates arguments and writes the agent + runtime defaults into the
// store. The defaults singleton is written only when absent so a re-run never
// clobbers an operator's customised defaults.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	agentName, _ := args["agent_name"].(string)
	if strings.TrimSpace(agentName) == "" {
		agentName = "default"
	}
	kind := strings.ToLower(strings.TrimSpace(stringArg(args, "kind")))
	command := strings.TrimSpace(stringArg(args, "command"))
	provider := strings.TrimSpace(stringArg(args, "provider"))
	agentArgs, err := coerceStringSlice(args["args"])
	if err != nil {
		return nil, fmt.Errorf("args: %w", err)
	}

	// Infer the kind when not given: provider ⇒ native, command ⇒ acp.
	if kind == "" {
		switch {
		case provider != "":
			kind = "native"
		case command != "":
			kind = "acp"
		}
	}

	var profile config.AgentProfile
	var resultKind string
	switch kind {
	case "native":
		profile, err = buildNative(args, provider)
		if err != nil {
			return nil, err
		}
		resultKind = "native"
	case "acp":
		if command == "" {
			return nil, errors.New("kind acp requires --command")
		}
		profile = config.AgentProfile{ACP: &config.ACPProfile{Command: command, Args: agentArgs}}
		resultKind = "acp"
	case "":
		// No agent configured. Stray agent flags are a mistake, not a silent skip.
		if hasNativeArgs(args) || command != "" || len(agentArgs) > 0 {
			return nil, errors.New("agent flags supplied but kind could not be determined; pass --kind, --command, or --provider")
		}
	default:
		return nil, fmt.Errorf("unknown kind %q (want native or acp)", kind)
	}

	s, err := t.store()
	if err != nil {
		return nil, err
	}
	if _, ok, err := s.GetSingleton(ctx, config.SingletonDefaults); err != nil {
		return nil, err
	} else if !ok {
		if err := s.PutSingleton(ctx, config.SingletonDefaults, runtimeDefaults); err != nil {
			return nil, err
		}
	}

	created := false
	if resultKind != "" {
		_, existed, err := s.GetItem(ctx, config.SectionAgent, agentName)
		if err != nil {
			return nil, err
		}
		created = !existed
		if err := s.UpsertItem(ctx, config.SectionAgent, agentName, profile); err != nil {
			return nil, err
		}
	}

	return Result{
		Created:   created,
		Enabled:   resultKind != "",
		AgentName: agentName,
		Kind:      resultKind,
	}, nil
}

// buildNative assembles and validates a native profile. provider is passed in
// already-trimmed since the caller used it for kind inference.
func buildNative(args map[string]any, provider string) (config.AgentProfile, error) {
	model := strings.TrimSpace(stringArg(args, "model"))
	apiKeyEnv := strings.TrimSpace(stringArg(args, "api_key_env"))
	switch {
	case provider == "":
		return config.AgentProfile{}, errors.New("kind native requires --provider")
	case model == "":
		return config.AgentProfile{}, errors.New("kind native requires --model")
	case apiKeyEnv == "":
		return config.AgentProfile{}, errors.New("kind native requires --api-key-env")
	}
	switch provider {
	case "gemini", "anthropic", "openai":
	default:
		return config.AgentProfile{}, fmt.Errorf("provider %q must be gemini, anthropic, or openai", provider)
	}
	tools, err := coerceStringSlice(args["tools"])
	if err != nil {
		return config.AgentProfile{}, fmt.Errorf("tools: %w", err)
	}
	mcpServers, err := coerceStringSlice(args["mcp_servers"])
	if err != nil {
		return config.AgentProfile{}, fmt.Errorf("mcp_servers: %w", err)
	}
	contextLimit, err := coerceInt(args["context_limit"])
	if err != nil {
		return config.AgentProfile{}, fmt.Errorf("context_limit: %w", err)
	}
	compaction := strings.ToLower(strings.TrimSpace(stringArg(args, "compaction")))
	switch compaction {
	case "", "truncate", "summarize":
	default:
		return config.AgentProfile{}, fmt.Errorf("compaction %q must be truncate or summarize", compaction)
	}
	cacheRetention := strings.ToLower(strings.TrimSpace(stringArg(args, "cache_retention")))
	switch cacheRetention {
	case "", "off", "none", "5m", "short", "1h", "long":
	default:
		return config.AgentProfile{}, fmt.Errorf("cache_retention %q must be one of 5m, 1h, or off", cacheRetention)
	}
	return config.AgentProfile{
		Tools:      tools,
		MCPServers: mcpServers,
		Native: &config.NativeProfile{
			Provider:         provider,
			Model:            model,
			BaseURL:          strings.TrimSpace(stringArg(args, "base_url")),
			APIKeyEnv:        apiKeyEnv,
			SystemPromptFile: strings.TrimSpace(stringArg(args, "system_prompt_file")),
			ContextLimit:     contextLimit,
			Compaction:       compaction,
			CacheRetention:   cacheRetention,
		},
	}, nil
}

func hasNativeArgs(args map[string]any) bool {
	for _, k := range []string{"provider", "model", "api_key_env", "base_url", "tools", "mcp_servers", "system_prompt_file", "compaction"} {
		if v, ok := args[k]; ok && v != nil {
			if s, isStr := v.(string); isStr && strings.TrimSpace(s) == "" {
				continue
			}
			return true
		}
	}
	return false
}

func stringArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}
