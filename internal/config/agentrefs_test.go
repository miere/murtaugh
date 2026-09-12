package config

import (
	"reflect"
	"testing"
)

func everyReferenceConfig() Config {
	return Config{
		Role: RoleNode,
		Agents: map[string]AgentProfile{
			"code":     {Native: &NativeProfile{Provider: "anthropic", Model: "claude", APIKeyEnv: "K"}},
			"tweaker":  {Native: &NativeProfile{Provider: "anthropic", Model: "claude", APIKeyEnv: "K"}},
			"reviewer": {Native: &NativeProfile{Provider: "anthropic", Model: "claude", APIKeyEnv: "K"}},
			"unfurler": {Native: &NativeProfile{Provider: "anthropic", Model: "claude", APIKeyEnv: "K"}},
			"flowbot":  {Native: &NativeProfile{Provider: "anthropic", Model: "claude", APIKeyEnv: "K"}},
		},
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
		WorkflowRules: map[string]WorkflowRuleConfig{
			"deploy": {
				RequestEvent: "interactive",
				Match:        map[string]any{"callback_id": "deploy"},
				Triggers: []TriggerConfig{
					{Type: "delegate-to-agent", DelegateToAgent: &DelegateToAgentConfig{Agent: "flowbot", Prompt: "go"}},
				},
			},
		},
		UnfurlRules: map[string]UnfurlRuleConfig{
			"tickets": {
				Match:  UnfurlMatchConfig{Domain: "example.com"},
				Unfurl: UnfurlActionConfig{DelegateToAgent: &DelegateToAgentConfig{Agent: "unfurler", Prompt: "summarise"}},
			},
		},
	}
}

// The path an operator would edit, not a Go field name, because the string goes into the message
// they read.
func TestAgentReferencesNamesEveryNameToBodySite(t *testing.T) {
	cfg := everyReferenceConfig()
	full := cfg
	full.OAuth = OAuthConfig{AppToken: "x", BotToken: "x"}
	if err := full.Validate(); err != nil {
		t.Fatalf("the fixture is not a valid configuration: %v", err)
	}

	want := []AgentReference{
		{Field: "chat.defaults.agent", Name: "code"},
		{Field: "chat.defaults.dm_agent", Name: "tweaker"},
		{Field: "chat.defaults.dm_agents[U1]", Name: "tweaker"},
		{Field: "chat.channels[nc-*].agent", Name: "reviewer"},
		{Field: "jobs[nightly].agent", Name: "code"},
		{Field: "workflow-rules[deploy].trigger[0].agent", Name: "flowbot"},
		{Field: "unfurl-rules[tickets].unfurl.agent", Name: "unfurler"},
	}
	got := cfg.AgentReferences()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AgentReferences()\n got %+v\nwant %+v", got, want)
	}
}

// Stable order, because the list is reported: a reordered journal entry for identical
// configurations would read as a change.
func TestAgentReferencesAreDeterministic(t *testing.T) {
	cfg := everyReferenceConfig()
	cfg.Chat.Defaults.DMAgents = map[string]string{"U3": "code", "U1": "tweaker", "U2": "reviewer"}
	first := cfg.AgentReferences()
	for i := 0; i < 20; i++ {
		if got := cfg.AgentReferences(); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs:\n got %+v\nwant %+v", i, got, first)
		}
	}
}

// A gateway not serving Slack chat has nothing to resolve chat names for; reporting them would
// warn about a feature nobody switched on.
func TestAgentReferencesSkipChatWhenChatIsOff(t *testing.T) {
	cfg := everyReferenceConfig()
	cfg.Chat.Enabled = false
	for _, ref := range cfg.AgentReferences() {
		if len(ref.Field) >= 4 && ref.Field[:4] == "chat" {
			t.Errorf("chat is off but %q is still reported", ref.Field)
		}
	}
}

// An empty served set is what a freshly restarted gateway has, which is why this must never be fatal.
func TestUnresolvedAgentsIsEverythingWhenNothingIsServed(t *testing.T) {
	refs := everyReferenceConfig().AgentReferences()
	if got := UnresolvedAgents(refs, nil); len(got) != len(refs) {
		t.Errorf("with no node attached %d of %d references resolved", len(refs)-len(got), len(refs))
	}
	served := []string{"code", "tweaker", "reviewer", "unfurler", "flowbot"}
	if got := UnresolvedAgents(refs, served); len(got) != 0 {
		t.Errorf("a fleet serving every name still reported %+v", got)
	}
}
