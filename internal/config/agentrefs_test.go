package config

import (
	"reflect"
	"testing"
)

// AgentReferences and the name→body checks in Validate are the same set seen
// from two sides, and #198 makes the second side load-bearing: once the gateway
// stops resolving those names, this list is the ONLY thing that carries them to
// the place that can. A site added to Validate and not here is a typo nothing
// ever reports.
//
// The configuration below exercises every one of the seven sites deliberately,
// so adding an eighth to Validate without adding it here fails this test.

func everyReferenceConfig() Config {
	return Config{
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

// Every site Validate checks appears in the list, with the configuration path an
// operator would edit rather than a Go field name — the string goes into the
// message they read.
func TestAgentReferencesNamesEveryNameToBodySite(t *testing.T) {
	cfg := everyReferenceConfig()
	// A sanity check that the fixture is actually a valid configuration: a
	// fixture that failed Validate would be exercising nothing.
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

// The order is stable, because the list is REPORTED: a journal entry that
// reordered itself between two identical configurations would read as a change
// to whoever queried it twice.
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

// Chat off means the chat names are not references: a gateway that is not
// serving Slack chat has nothing to resolve them for, and reporting them as
// unservable would be a warning about a feature nobody switched on.
func TestAgentReferencesSkipChatWhenChatIsOff(t *testing.T) {
	cfg := everyReferenceConfig()
	cfg.Chat.Enabled = false
	for _, ref := range cfg.AgentReferences() {
		if len(ref.Field) >= 4 && ref.Field[:4] == "chat" {
			t.Errorf("chat is off but %q is still reported", ref.Field)
		}
	}
}

// The connect-time half. An empty served set yields every reference, which is
// what a gateway with an empty registry has and is exactly why this can never be
// fatal.
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
