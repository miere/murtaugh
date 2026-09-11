package config

import (
	"strings"
	"testing"
)

func combinedConfig() Config {
	return Config{
		OAuth: OAuthConfig{AppToken: "xapp-1", BotToken: "xoxb-1"},
		Agents: map[string]AgentProfile{
			"code": {Native: &NativeProfile{Provider: "anthropic", Model: "claude", APIKeyEnv: "ANTHROPIC_API_KEY"}},
		},
		Chat: ChatConfig{Enabled: true, Defaults: ChatDefaults{Agent: "code"}},
	}
}

func TestCombinedRoleStillRequiresTokensAndResolvesNames(t *testing.T) {
	cfg := combinedConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a valid combined configuration was refused: %v", err)
	}

	missingTokens := combinedConfig()
	missingTokens.OAuth = OAuthConfig{}
	if err := missingTokens.Validate(); err == nil {
		t.Error("a combined configuration validated without Slack credentials")
	}

	typo := combinedConfig()
	typo.Chat.Defaults.Agent = "cdoe"
	err := typo.Validate()
	if err == nil || !strings.Contains(err.Error(), "not found in agents.yaml") {
		t.Errorf("a typo'd default agent gave %v; the write-time check must still fire for a combined install", err)
	}
}

// A gateway holds no profile bodies, so its name checks can only run when a node says what it serves.
func TestGatewayRoleDefersEveryNameToBodyCheck(t *testing.T) {
	cfg := Config{
		Role:  RoleGateway,
		OAuth: OAuthConfig{AppToken: "xapp-1", BotToken: "xoxb-1"},
		Chat: ChatConfig{
			Enabled: true,
			Defaults: ChatDefaults{
				Agent:    "code",
				DMAgent:  "tweaker",
				DMAgents: map[string]string{"U1": "tweaker"},
			},
			Channels: ChannelRules{{Match: "nc-*", Agent: "reviewer"}},
		},
		Jobs: map[string]JobProfile{
			"nightly": {Agent: "code", Prompt: "sweep", Schedule: "0 3 * * *"},
		},
		UnfurlRules: map[string]UnfurlRuleConfig{
			"tickets": {
				Match:  UnfurlMatchConfig{Domain: "example.com"},
				Unfurl: UnfurlActionConfig{DelegateToAgent: &DelegateToAgentConfig{Agent: "code", Prompt: "summarise"}},
			},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a broker gateway cannot validate its own configuration: %v", err)
	}

	blank := cfg
	blank.Chat.Defaults.Agent = ""
	if err := blank.Validate(); err == nil {
		t.Error("a gateway accepted a blank chat.defaults.agent; only the BODY lookup was supposed to move")
	}

	bad := cfg
	bad.Chat.Channels = ChannelRules{{Match: "nc-[*", Agent: "code"}}
	if err := bad.Validate(); err == nil {
		t.Error("a gateway accepted a malformed channel glob")
	}
}

// A blank name used to be caught only by the deferred body lookup, and AgentReferences skips
// blanks, so it fell through both halves; it needs no body, so every role must refuse it.
func TestABlankAgentNameIsRefusedAtEverySiteAndEveryRole(t *testing.T) {
	base := Config{
		Agents: map[string]AgentProfile{
			"code": {Native: &NativeProfile{Provider: "anthropic", Model: "claude", APIKeyEnv: "K"}},
		},
		OAuth: OAuthConfig{AppToken: "x", BotToken: "x"},
		Chat:  ChatConfig{Enabled: true, Defaults: ChatDefaults{Agent: "code"}},
	}
	for name, blank := range map[string]func(*Config){
		"chat.defaults.dm_agents": func(c *Config) {
			c.Chat.Defaults.DMAgents = map[string]string{"U1": "   "}
		},
		"chat.defaults.dm_agent": func(c *Config) { c.Chat.Defaults.DMAgent = "   " },
		"chat.channels[].agent": func(c *Config) {
			c.Chat.Channels = ChannelRules{{Match: "nc-*", Agent: "   "}}
		},
	} {
		for _, role := range []Role{RoleCombined, RoleGateway, RoleNode} {
			cfg := base
			cfg.Role = role
			blank(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Errorf("a %s accepted a blank %s; blankness needs no profile body to detect, "+
					"and AgentReferences skips blanks, so nothing reports it at connect time either", role, name)
			}
		}
	}

	cfg := base
	cfg.Chat.Defaults.DMAgents = map[string]string{"U1": "   "}
	for _, ref := range cfg.AgentReferences() {
		if strings.TrimSpace(ref.Name) == "" {
			t.Errorf("AgentReferences reported the blank at %s; Validate already has it", ref.Field)
		}
	}
}

// A node has no Slack connection, so requiring the workspace's tokens of it would hand them to
// every laptop that runs an agent.
func TestNodeRoleNeedsNoSlackCredentialsAndStillResolvesNames(t *testing.T) {
	cfg := Config{
		Role: RoleNode,
		Agents: map[string]AgentProfile{
			"code": {Native: &NativeProfile{Provider: "anthropic", Model: "claude", APIKeyEnv: "ANTHROPIC_API_KEY"}},
		},
		Chat: ChatConfig{Enabled: true, Defaults: ChatDefaults{Agent: "code"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a node cannot validate without Slack credentials: %v", err)
	}

	typo := cfg
	typo.Chat.Defaults.Agent = "cdoe"
	if err := typo.Validate(); err == nil {
		t.Error("a node accepted a default agent it holds no body for; a node DOES hold bodies and must still check")
	}
}

// Checked at load, not dial time: the dialler's refusal lands inside a patient redial loop that
// would back off forever with one log line explaining why.
func TestNodeSeedAddressesAreCheckedAtLoad(t *testing.T) {
	for _, address := range []string{"https://gateway.example.com", "gateway.example.com", "wss://"} {
		cfg := Config{Role: RoleNode, Node: NodeConfig{Gateway: []string{address}}}
		if err := cfg.Validate(); err == nil {
			t.Errorf("%q was accepted as a gateway seed address", address)
		}
	}
	ok := Config{Role: RoleNode, Node: NodeConfig{Gateway: []string{
		"wss://gateway.example.com:8443", "ws://127.0.0.1:8787",
	}}}
	if err := ok.Validate(); err != nil {
		t.Errorf("valid seed addresses were refused: %v", err)
	}
}
