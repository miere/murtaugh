package config

import (
	"strings"
	"testing"
)

// The role decides which of Validate's rules apply, and these are the three
// answers it can give. What they protect is stated in role.go; what they must
// not do is change anything for the combined install that ships today, which is
// the first test below.

// combined is one process holding both halves, and it is the zero value of Role
// precisely so nothing existing has to name a role to keep working.
func combinedConfig() Config {
	return Config{
		OAuth: OAuthConfig{AppToken: "xapp-1", BotToken: "xoxb-1"},
		Agents: map[string]AgentProfile{
			"code": {Native: &NativeProfile{Provider: "anthropic", Model: "claude", APIKeyEnv: "ANTHROPIC_API_KEY"}},
		},
		Chat: ChatConfig{Enabled: true, Defaults: ChatDefaults{Agent: "code"}},
	}
}

// The shipping default must be untouched: tokens required, names resolved
// against bodies, exactly as before this item.
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

// A gateway holds Slack credentials and no profile bodies, so a name it uses can
// only be checked when a node says what it serves. This is the whole of #198's
// behavioural change, seen at the place it was removed from.
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

	// Everything that is NOT a name→body check still runs. A blank name needs no
	// body to detect and is still an error for every role.
	blank := cfg
	blank.Chat.Defaults.Agent = ""
	if err := blank.Validate(); err == nil {
		t.Error("a gateway accepted a blank chat.defaults.agent; only the BODY lookup was supposed to move")
	}

	// And a malformed glob, which is likewise answerable without a body.
	bad := cfg
	bad.Chat.Channels = ChannelRules{{Match: "nc-[*", Agent: "code"}}
	if err := bad.Validate(); err == nil {
		t.Error("a gateway accepted a malformed channel glob")
	}
}

// TestABlankAgentNameIsRefusedAtEverySiteAndEveryRole is the same sentence as
// the two blanks above, applied to the three sites that were only ever checked
// by accident.
//
// A blank name used to be caught at those sites by the BODY lookup failing —
// `dm_agents[U1] "  " not found in agents` — which is a name→body check, so
// #198 deferred it along with the rest. AgentReferences then skips blanks on
// purpose, to avoid naming the same problem twice, so nothing reported them at
// connect time either: a blank fell through both halves and a combined install
// silently stopped agreeing with a gateway about the same file.
//
// Every case here is one no body is needed to answer, which is the line the
// split is drawn on.
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

	// The blank must not be reported twice. AgentReferences skips it precisely so
	// the connect-time check does not name a problem Validate already named.
	cfg := base
	cfg.Chat.Defaults.DMAgents = map[string]string{"U1": "   "}
	for _, ref := range cfg.AgentReferences() {
		if strings.TrimSpace(ref.Name) == "" {
			t.Errorf("AgentReferences reported the blank at %s; Validate already has it", ref.Field)
		}
	}
}

// A node has no Slack connection, so requiring the workspace's tokens of it
// would mean handing them to every laptop that runs an agent. It DOES hold
// bodies, so its own name checks keep running.
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

// The seed addresses are validated where they are configured rather than at dial
// time: the dialler's refusal arrives inside a redial loop designed to be
// patient, so a bad scheme would back off forever with one log line explaining
// why, scrolled past.
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
